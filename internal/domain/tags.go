// Package domain is the Single Source of Truth (SSOT) for antic's domain
// vocabulary types built on top of internal/types:
//
//  1. tags.go:       Phantom tags & Typestate state indices
//  2. predicates.go: Refinement predicates, Quotient normalizers, and orderings
//  3. proofs.go:     Ghosts of Departed Proofs (GDP) propositions & proof rules (Prove*)
package domain

import "github.com/RNG999/antic/internal/types"

// KernelSigTag tags a kernel+initrd+cmdline identity signature.
type KernelSigTag struct{}

// KernelSig is a phantom-typed kernel identity signature.
type KernelSig = types.Phantom[string, KernelSigTag]

// NewKernelSig wraps a kernel signature string.
func NewKernelSig(sig string) KernelSig { return types.NewPhantom[string, KernelSigTag](sig) }

// TextTag tags human-readable log or error reason messages.
type TextTag struct{}

// Text is a phantom-typed human-readable message string.
type Text = types.Phantom[string, TextTag]

// NewText wraps a message string.
func NewText(s string) Text { return types.NewPhantom[string, TextTag](s) }

// PeerCIDTag tags a vsock peer Context ID.
type PeerCIDTag struct{}

// PeerCID is a phantom-typed vsock peer CID.
type PeerCID = types.Phantom[uint32, PeerCIDTag]

// NewPeerCID wraps a raw vsock CID.
func NewPeerCID(cid uint32) PeerCID { return types.NewPhantom[uint32, PeerCIDTag](cid) }

// UnixSecTag tags a Unix timestamp in seconds.
type UnixSecTag struct{}

// UnixSec is a phantom-typed Unix timestamp in seconds.
type UnixSec = types.Phantom[int64, UnixSecTag]

// NewUnixSec wraps a Unix timestamp in seconds.
func NewUnixSec(sec int64) UnixSec { return types.NewPhantom[int64, UnixSecTag](sec) }

// Typestate tags for NixOS StorePath roles in the guest lifecycle:
//   - InstalledState: installed in the guest's /nix/store
//   - CandidateState: pending live activation via switch-to-configuration
type (
	InstalledState struct{}
	CandidateState struct{}
)

// InstalledStorePath is a typestate-indexed StorePath representing the installed closure.
type InstalledStorePath = types.Indexed[StorePath, InstalledState]

// CandidateStorePath is a typestate-indexed StorePath representing an in-flight switch target.
type CandidateStorePath = types.Indexed[StorePath, CandidateState]
