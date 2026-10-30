// Package egress implements the pure Sans-I/O & TEA state machine for guest
// DNS domain filtering and dynamic nftables allow-set updates.
//
// All fields in Model, Msg, and Effect use formal type-theory combinators from
// internal/typetheory and domain vocabulary types from internal/domain (SSOT):
//   - Refinement & Quotient Types (domain.UsableAddr, domain.DomainName,
//     domain.DomainPattern, domain.BoundedTimeout)
//   - Phantom Types (domain.UnixSec)
//   - Canonical Quotient Containers (types.SortedSet for Allow, Deny, SetEntries, RefusedLog)
//   - NonEmpty Lists (types.NonEmpty[SetElementUpdate] in EffectUpdateNFTSet so
//     empty nftables transactions are unrepresentable at the type level).
package egress

import (
	"errors"
	"fmt"
	"slices"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/types"
)

// SetEntry records one allowed IP address and its expiration time in the nftables set.
type SetEntry struct {
	Addr      domain.UsableAddr
	ExpiresAt domain.UnixSec
}

// SetEntryByAddr defines the canonical total order on SetEntry by its unique UsableAddr key.
type SetEntryByAddr struct{}

// Key extracts the unique UsableAddr of a SetEntry.
func (SetEntryByAddr) Key(e SetEntry) domain.UsableAddr { return e.Addr }

// Less orders two UsableAddr keys using netip.Addr.Compare.
func (SetEntryByAddr) Less(a, b domain.UsableAddr) bool {
	return a.Get().Compare(b.Get()) < 0
}

// PatternSet is a canonical SortedSet of DomainPatterns.
type PatternSet = types.SortedSet[domain.DomainPattern, domain.DomainPattern, domain.DomainPatternOrder]

// DomainSet is a canonical SortedSet of DomainNames.
type DomainSet = types.SortedSet[domain.DomainName, domain.DomainName, domain.DomainNameOrder]

// EntrySet is a canonical SortedSet of SetEntries ordered by Addr.
type EntrySet = types.SortedSet[SetEntry, domain.UsableAddr, SetEntryByAddr]

// AnswerCandidate represents one resolved address and its bounded timeout from a DNS response.
type AnswerCandidate struct {
	Addr    domain.UsableAddr
	Timeout domain.BoundedTimeout
}

// SetElementUpdate represents one nftables set element refresh command.
type SetElementUpdate struct {
	Addr    domain.UsableAddr
	Timeout domain.BoundedTimeout
}

// Default decides what happens to destinations no allow entry names
// (network.default in the guest config).
type Default uint8

const (
	// DefaultAllow reaches the mode's base set; allow entries add exceptions.
	DefaultAllow Default = iota
	// DefaultDeny reaches only allow entries (and addresses learned by
	// resolving allowed domains).
	DefaultDeny
)

// Model is the immutable, normalized (SSOT) state of the DNS egress filter.
type Model struct {
	Default    Default
	Allow      PatternSet
	Deny       PatternSet
	SetEntries EntrySet
	RefusedLog DomainSet
}

// Init constructs a normalized initial Model from the default and validated
// Allow and Deny patterns.
func Init(def Default, allow, deny []domain.DomainPattern) Model {
	return Model{
		Default: def,
		Allow:   types.BuildSet[domain.DomainPattern, domain.DomainPattern, domain.DomainPatternOrder](allow),
		Deny:    types.BuildSet[domain.DomainPattern, domain.DomainPattern, domain.DomainPatternOrder](deny),
	}
}

// DomainAllowed reports whether domain is permitted by Model's Deny and Allow lists.
// Deny always takes precedence; with DefaultDeny, domain must match Allow.
func (m Model) DomainAllowed(d domain.DomainName) bool {
	if !d.Valid() {
		return false
	}
	for _, p := range m.Deny.All() {
		if domain.DomainMatches(d, p) {
			return false
		}
	}
	if m.Default == DefaultDeny {
		for _, p := range m.Allow.All() {
			if domain.DomainMatches(d, p) {
				return true
			}
		}
		return false
	}
	return true
}

// Invariant checks the inductive safety invariants of Model.
func Invariant(m Model) error {
	if !m.Allow.Canonical() || !m.Deny.Canonical() || !m.SetEntries.Canonical() || !m.RefusedLog.Canonical() {
		return errors.New("invariant violation: SortedSet is not in canonical order")
	}
	for _, p := range m.Allow.All() {
		if !p.Valid() {
			return fmt.Errorf("invariant violation: invalid Allow pattern %q", p)
		}
	}
	for _, p := range m.Deny.All() {
		if !p.Valid() {
			return fmt.Errorf("invariant violation: invalid Deny pattern %q", p)
		}
	}
	for _, e := range m.SetEntries.All() {
		if !e.Addr.Valid() {
			return fmt.Errorf("invariant violation: invalid UsableAddr in SetEntries: %v", e.Addr)
		}
	}
	if m.RefusedLog.Len() > 1024 {
		return fmt.Errorf("invariant violation: RefusedLog exceeds 1024 bound (%d)", m.RefusedLog.Len())
	}
	for _, d := range m.RefusedLog.All() {
		if !d.Valid() {
			return fmt.Errorf("invariant violation: invalid DomainName in RefusedLog: %q", d)
		}
	}
	return nil
}

// FrameValid enforces the Separation Logic Frame Rule ({P} C {Q} => {P * R} C {Q * R}):
//   - Allow and Deny policy sets are an immutable frame across all transitions.
//   - MsgEvaluateQuery only modifies RefusedLog (leaving SetEntries untouched).
//   - MsgRecordAnswers only modifies SetEntries (leaving RefusedLog untouched).
func FrameValid(prev Model, msg Msg, next Model) error {
	if prev.Default != next.Default || !slices.Equal(prev.Allow.All(), next.Allow.All()) || !slices.Equal(prev.Deny.All(), next.Deny.All()) {
		return errors.New("frame violation: Allow/Deny policy frame mutated")
	}
	switch msg.(type) {
	case MsgEvaluateQuery:
		if !slices.Equal(prev.SetEntries.All(), next.SetEntries.All()) {
			return errors.New("frame violation: MsgEvaluateQuery modified SetEntries frame")
		}
	case MsgRecordAnswers:
		if !slices.Equal(prev.RefusedLog.All(), next.RefusedLog.All()) {
			return errors.New("frame violation: MsgRecordAnswers modified RefusedLog frame")
		}
	}
	return nil
}

// StepValid checks the IronFleet / TLA+ transition relation predicate across
// a single step (prev, msg) -> (next, effects).
func StepValid(prev Model, msg Msg, next Model, effects []Effect) error {
	if prev.Default != next.Default || prev.Allow.Len() != next.Allow.Len() || prev.Deny.Len() != next.Deny.Len() {
		return errors.New("step violation: Allow or Deny policy mutated during transition")
	}
	for _, eff := range effects {
		switch e := eff.(type) {
		case EffectForwardQuery:
			if !prev.DomainAllowed(e.Domain) {
				return fmt.Errorf("step violation: EffectForwardQuery emitted for disallowed domain %q", e.Domain)
			}
		case EffectUpdateNFTSet:
			if prev.Default != DefaultDeny {
				return errors.New("step violation: EffectUpdateNFTSet emitted without default deny (there is no learned set)")
			}
			for _, u := range e.Updates.Slice() {
				if !u.Addr.Valid() || !u.Timeout.Valid() {
					return fmt.Errorf("step violation: invalid SetElementUpdate %+v", u)
				}
			}
		}
	}
	return nil
}

// QueryDecision classifies the DNS query type/class for policy evaluation.
type QueryDecision uint8

const (
	// QueryTypeAddress indicates an IN-class A or AAAA query.
	QueryTypeAddress QueryDecision = iota
	// QueryTypeOtherIN indicates an IN-class query for a non-address record (returns NODATA).
	QueryTypeOtherIN
	// QueryClassNonIN indicates a non-IN class query (returns REFUSED).
	QueryClassNonIN
)

// ReplyCode represents the DNS reply action to send back to the guest.
type ReplyCode uint8

const (
	// ReplyNoData sends an empty NOERROR (rcode 0) answer.
	ReplyNoData ReplyCode = iota
	// ReplyRefused sends REFUSED (rcode 5).
	ReplyRefused
)

// Msg is the closed sum type of inputs to the DNS egress state machine.
type Msg interface {
	isMsg()
}

// MsgEvaluateQuery evaluates an incoming client DNS query against the domain policy.
type MsgEvaluateQuery struct {
	Domain domain.DomainName
	Kind   QueryDecision
}

func (MsgEvaluateQuery) isMsg() {}

// MsgRecordAnswers processes validated usable addresses from an upstream DNS response
// at timestamp Now and computes any required nftables set updates.
type MsgRecordAnswers struct {
	Now     domain.UnixSec
	Answers []AnswerCandidate
}

func (MsgRecordAnswers) isMsg() {}

// Effect is the closed sum type of side effects to be executed by the egress runtime.
type Effect interface {
	isEffect()
}

// EffectReplyImmediate instructs the runtime to return an immediate DNS response
// without querying upstream.
type EffectReplyImmediate struct {
	Code ReplyCode
}

func (EffectReplyImmediate) isEffect() {}

// EffectLogRefused instructs the runtime to log the first refusal of Domain.
type EffectLogRefused struct {
	Domain domain.DomainName
}

func (EffectLogRefused) isEffect() {}

// EffectForwardQuery instructs the runtime to forward the query for Domain to the
// upstream resolver.
type EffectForwardQuery struct {
	Domain domain.DomainName
}

func (EffectForwardQuery) isEffect() {}

// EffectUpdateNFTSet instructs the runtime to atomically add/refresh a guaranteed
// NonEmpty batch of Updates in nftables.
type EffectUpdateNFTSet struct {
	Updates types.NonEmpty[SetElementUpdate]
}

func (EffectUpdateNFTSet) isEffect() {}

// Update is the pure, total, deterministic transition function:
// (Model, Msg) -> (Model, []Effect).
func Update(m Model, msg Msg) (Model, []Effect) {
	next := Model{
		Default:    m.Default,
		Allow:      m.Allow.Clone(),
		Deny:       m.Deny.Clone(),
		SetEntries: m.SetEntries.Clone(),
		RefusedLog: m.RefusedLog.Clone(),
	}
	var effects []Effect

	switch ev := msg.(type) {
	case MsgEvaluateQuery:
		if !ev.Domain.Valid() || ev.Kind == QueryClassNonIN || !next.DomainAllowed(ev.Domain) {
			if ev.Domain.Valid() && !next.RefusedLog.Contains(ev.Domain) && next.RefusedLog.Len() < 1024 {
				next.RefusedLog = next.RefusedLog.Upsert(ev.Domain)
				effects = append(effects, EffectLogRefused{Domain: ev.Domain})
			}
			effects = append(effects, EffectReplyImmediate{Code: ReplyRefused})
			return next, effects
		}
		if ev.Kind == QueryTypeOtherIN {
			return next, []Effect{EffectReplyImmediate{Code: ReplyNoData}}
		}
		return next, []Effect{EffectForwardQuery{Domain: ev.Domain}}

	case MsgRecordAnswers:
		if next.Default != DefaultDeny {
			return next, nil
		}
		now := ev.Now.Get()
		var updates []SetElementUpdate
		for _, ans := range ev.Answers {
			if !ans.Addr.Valid() || !ans.Timeout.Valid() {
				continue
			}
			tSecs := int64(ans.Timeout.Get())
			if existing, ok := next.SetEntries.Lookup(ans.Addr); ok {
				rem := existing.ExpiresAt.Get() - now
				if rem > tSecs/2 {
					continue
				}
			}
			next.SetEntries = next.SetEntries.Upsert(SetEntry{
				Addr:      ans.Addr,
				ExpiresAt: domain.NewUnixSec(now + tSecs),
			})
			updates = append(updates, SetElementUpdate{
				Addr:    ans.Addr,
				Timeout: ans.Timeout,
			})
		}
		if next.SetEntries.Len() > 4096 {
			var kept []SetEntry
			for _, e := range next.SetEntries.All() {
				if e.ExpiresAt.Get() >= now {
					kept = append(kept, e)
				}
			}
			next.SetEntries = types.BuildSet[SetEntry, domain.UsableAddr, SetEntryByAddr](kept)
		}
		if ne, ok := types.ParseNonEmpty(updates); ok {
			effects = append(effects, EffectUpdateNFTSet{Updates: ne})
		}
		return next, effects
	}

	return next, nil
}
