package guest

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// Built from bpf/userns.bpf.c by the Nix derivation before "go build".
//
//go:embed bpf/userns.bpf.o
var usernsObject []byte

// The link is pinned so the restriction outlives this process; removing the
// pin detaches it.
const usernsPin = "/sys/fs/bpf/hanare-userns"

func restrictUserns() error {
	if err := requireBPFLSM(); err != nil {
		return err
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(usernsObject))
	if err != nil {
		return err
	}
	var objs struct {
		Prog *ebpf.Program `ebpf:"restrict_userns"`
	}
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		return fmt.Errorf("load: %w", err)
	}
	defer objs.Prog.Close()
	l, err := link.AttachLSM(link.LSMOptions{Program: objs.Prog})
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	defer l.Close()
	if err := os.RemoveAll(usernsPin); err != nil {
		return err
	}
	if err := os.MkdirAll(usernsPin, 0o700); err != nil {
		return err
	}
	return l.Pin(filepath.Join(usernsPin, "link"))
}
