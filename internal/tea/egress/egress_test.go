package egress_test

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/tea/egress"
)

func TestExhaustiveStateSpaceAndIncorrectnessReachability(t *testing.T) {
	pGit, _ := domain.ParseDomainPattern("*.github.com")
	pAnthropic, _ := domain.ParseDomainPattern("anthropic.com")
	pGist, _ := domain.ParseDomainPattern("gist.github.com")

	initModel := egress.Init(
		[]domain.DomainPattern{pGit, pAnthropic},
		[]domain.DomainPattern{pGist},
	)
	if err := egress.Invariant(initModel); err != nil {
		t.Fatalf("Init invariant failed: %v", err)
	}

	dAPI, _ := domain.ParseDomainName("api.github.com")
	dGist, _ := domain.ParseDomainName("gist.github.com")
	dEvil, _ := domain.ParseDomainName("evil.com")
	a1, _ := domain.ParseUsableAddr(netip.MustParseAddr("140.82.114.6"))
	a2, _ := domain.ParseUsableAddr(netip.MustParseAddr("2606:4700::1"))

	alphabet := []egress.Msg{
		egress.MsgEvaluateQuery{Domain: dAPI, Kind: egress.QueryTypeAddress},
		egress.MsgEvaluateQuery{Domain: dAPI, Kind: egress.QueryTypeOtherIN},
		egress.MsgEvaluateQuery{Domain: dAPI, Kind: egress.QueryClassNonIN},
		egress.MsgEvaluateQuery{Domain: dGist, Kind: egress.QueryTypeAddress},
		egress.MsgEvaluateQuery{Domain: dEvil, Kind: egress.QueryTypeAddress},
		egress.MsgRecordAnswers{
			Now: domain.NewUnixSec(1000),
			Answers: []egress.AnswerCandidate{
				{Addr: a1, Timeout: domain.TimeoutFromTTL(60)},
				{Addr: a2, Timeout: domain.TimeoutFromTTL(3600)},
			},
		},
		egress.MsgRecordAnswers{
			Now: domain.NewUnixSec(1500),
			Answers: []egress.AnswerCandidate{
				{Addr: a1, Timeout: domain.TimeoutFromTTL(60)},
			},
		},
	}

	stateKey := func(m egress.Model) string {
		return fmt.Sprintf("%#v", m)
	}

	visited := map[string]bool{stateKey(initModel): true}
	frontier := []egress.Model{initModel}
	transitions := 0
	diameter := 0

	// Incorrectness Logic (O'Hearn POPL 2020) under-approximate reachability witnesses:
	var (
		reachedReplyImmediate bool
		reachedForwardQuery   bool
		reachedLogRefused     bool
		reachedUpdateNFTSet   bool
	)

	for len(frontier) > 0 {
		var nextFrontier []egress.Model
		for _, s := range frontier {
			for _, msg := range alphabet {
				transitions++
				next, effs := egress.Update(s, msg)

				// 1. Hoare Logic Over-Approximate Safety:
				if err := egress.Invariant(next); err != nil {
					t.Fatalf("Invariant failed on %+v: %v", msg, err)
				}
				// 2. Separation Logic Frame Rule:
				if err := egress.FrameValid(s, msg, next); err != nil {
					t.Fatalf("FrameValid failed on %+v: %v", msg, err)
				}
				// 3. IronFleet / TLA+ Two-State Transition Predicate:
				if err := egress.StepValid(s, msg, next, effs); err != nil {
					t.Fatalf("StepValid failed on %+v: %v", msg, err)
				}

				for _, eff := range effs {
					switch eff.(type) {
					case egress.EffectReplyImmediate:
						reachedReplyImmediate = true
					case egress.EffectForwardQuery:
						reachedForwardQuery = true
					case egress.EffectLogRefused:
						reachedLogRefused = true
					case egress.EffectUpdateNFTSet:
						reachedUpdateNFTSet = true
					}
				}

				k := stateKey(next)
				if !visited[k] {
					visited[k] = true
					nextFrontier = append(nextFrontier, next)
				}
			}
		}
		if len(nextFrontier) > 0 {
			diameter++
		}
		frontier = nextFrontier
	}

	const maxAllowedStates = 32
	t.Logf("egress reachable state space: states=%d (bound=%d), transitions=%d, diameter=%d",
		len(visited), maxAllowedStates, transitions, diameter)
	if len(visited) > maxAllowedStates {
		t.Fatalf("state-space explosion gate: reachable states %d > bound %d", len(visited), maxAllowedStates)
	}

	// Incorrectness Logic: every postcondition / effect branch must be reachable.
	if !reachedReplyImmediate || !reachedForwardQuery || !reachedLogRefused || !reachedUpdateNFTSet {
		t.Fatalf("Incorrectness Logic reachability failure: reply=%v forward=%v refused=%v nft=%v",
			reachedReplyImmediate, reachedForwardQuery, reachedLogRefused, reachedUpdateNFTSet)
	}
}
