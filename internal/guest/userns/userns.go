// Package userns loads banken's BPF LSM program that denies user namespaces
// to everyone but root (bpf/userns.bpf.c).
package userns

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/RNG999/hanare/internal/guest/sd"
)

// Built from bpf/userns.bpf.c by the Nix derivation before "go build".
//
//go:embed bpf/userns.bpf.o
var object []byte

// pinDir holds the pinned link, so the restriction outlives the loader.
const pinDir = "/sys/fs/bpf/banken-restrict-userns"

// Restrict attaches the program and pins its link. bpffs has no rename, so
// a new pin gets its own name and older pins are removed afterwards: there
// is no moment without the restriction.
func Restrict() error {
	if err := sd.RequireBPFLSM(); err != nil {
		return err
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(object))
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
	if err := os.MkdirAll(pinDir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("link-%d", time.Now().UnixNano())
	if err := l.Pin(filepath.Join(pinDir, name)); err != nil {
		return fmt.Errorf("pin: %w", err)
	}
	old, _ := filepath.Glob(filepath.Join(pinDir, "link-*"))
	for _, p := range old {
		if filepath.Base(p) != name {
			os.Remove(p)
		}
	}
	return nil
}
