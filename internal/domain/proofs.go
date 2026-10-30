package domain

import (
	"fmt"

	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/types"
)

// HostAuthProp is the proposition `PeerCID == proto.HostCID`.
type HostAuthProp struct{}

// HostAuthProof is an unforgeable GDP witness proving that a vsock connection
// originated from the host (CID 2).
type HostAuthProof = types.Proof[HostAuthProp]

// ProveHostCID verifies that cid equals proto.HostCID and mints a HostAuthProof.
func ProveHostCID(cid PeerCID) (HostAuthProof, error) {
	if cid.Get() != proto.HostCID {
		return HostAuthProof{}, fmt.Errorf("refused agent connection from CID %d", cid.Get())
	}
	return types.Axiom[HostAuthProp](), nil
}

// WithinHomeProp is the relational proposition `WithinSubtree(Home(User), Path)`.
type WithinHomeProp struct{}

// WithinHomeProof is an unforgeable GDP witness proving that a mount target
// path lies strictly beneath the guest user's home directory.
type WithinHomeProof = types.Proof[WithinHomeProp]

// ProveWithinHome verifies that target lies strictly beneath home and mints a
// WithinHomeProof.
func ProveWithinHome(home, target AbsPath) (WithinHomeProof, error) {
	if !WithinSubtree(home, target) {
		return WithinHomeProof{}, fmt.Errorf("mount point %q is not a clean path below %s", target.Get(), home.Get())
	}
	return types.Axiom[WithinHomeProp](), nil
}
