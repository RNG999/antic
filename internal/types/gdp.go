package types

// Proof represents a Ghost of a Departed Proof (Matt Noonan, Haskell Symposium
// 2018; Necula 1997 Proof-Carrying Code): a zero-payload, unforgeable witness
// proving that proposition Prop holds.
//
// Because `proven` is unexported, external packages cannot forge a valid
// Proof[Prop]{proven: true}; a valid Proof can only be minted via Axiom in
// internal/domain proof rules (enforced by internal/arch linter).
type Proof[Prop any] struct {
	proven bool
}

// Valid reports whether the proof witness was legitimately constructed by a
// proof rule.
func (p Proof[Prop]) Valid() bool { return p.proven }

// Axiom mints a Proof[Prop] for a verified proposition. The architecture linter
// restricts calls to Axiom exclusively to proof-rule functions (Prove*) inside
// internal/domain/proofs.go.
func Axiom[Prop any]() Proof[Prop] {
	return Proof[Prop]{proven: true}
}
