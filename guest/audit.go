package guest

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
)

// Built from bpf/audit.bpf.c by the Nix derivation before "go build".
//
//go:embed bpf/audit.bpf.o
var auditObject []byte

// event mirrors struct event in bpf/audit.bpf.c.
type event struct {
	Cgroup uint64
	Type   uint32
	PID    uint32
	PPID   uint32
	UID    uint32
	Family uint16
	Port   uint16 // network byte order
	Addr   [16]byte
	Comm   [16]byte
	Path   [256]byte
	_      uint32
}

// record is one JSON line in the audit log.
type record struct {
	Time    string `json:"time"`
	Session string `json:"session,omitempty"`
	Event   string `json:"event"`
	PID     uint32 `json:"pid"`
	PPID    uint32 `json:"ppid,omitempty"`
	Comm    string `json:"comm"`
	Path    string `json:"path,omitempty"`
	Argv    string `json:"argv,omitempty"`
	Dest    string `json:"dest,omitempty"`
}

func audit(uid uint32) error {
	if err := requireBPFLSM(); err != nil {
		return err
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(auditObject))
	if err != nil {
		return err
	}
	if err := spec.Variables["target_uid"].Set(uid); err != nil {
		return err
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	defer coll.Close()
	for name, prog := range coll.Programs {
		var l link.Link
		switch prog.Type() {
		case ebpf.LSM:
			l, err = link.AttachLSM(link.LSMOptions{Program: prog})
		default:
			l, err = link.AttachTracing(link.TracingOptions{Program: prog})
		}
		if err != nil {
			return fmt.Errorf("attach %s: %w", name, err)
		}
		defer l.Close()
	}

	rd, err := ringbuf.NewReader(coll.Maps["events"])
	if err != nil {
		return err
	}
	defer rd.Close()
	sessions := newSessionNames()
	enc := json.NewEncoder(os.Stdout)
	for {
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return nil
			}
			return err
		}
		var e event
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
			continue
		}
		r := record{
			Time:    time.Now().UTC().Format(time.RFC3339Nano),
			Session: sessions.lookup(e.Cgroup),
			PID:     e.PID,
			Comm:    cstr(e.Comm[:]),
		}
		switch e.Type {
		case 1:
			r.Event, r.PPID, r.Path = "exec", e.PPID, cstr(e.Path[:])
			if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", e.PID)); err == nil {
				r.Argv = strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
			}
		case 2:
			r.Event = "connect"
			port := e.Port>>8 | e.Port<<8
			if e.Family == syscall.AF_INET {
				r.Dest = netip.AddrPortFrom(netip.AddrFrom4([4]byte(e.Addr[:4])), port).String()
			} else {
				r.Dest = netip.AddrPortFrom(netip.AddrFrom16(e.Addr), port).String()
			}
		case 3:
			r.Event, r.Path = "write-open", cstr(e.Path[:])
		default:
			continue
		}
		enc.Encode(r)
	}
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// sessionNames maps cgroup IDs to the systemd unit they belong to (e.g. a
// hanare-ssh@ instance, i.e. one "hanare run"). A cgroup's ID is the inode
// number of its directory.
type sessionNames map[uint64]string

func newSessionNames() sessionNames { return sessionNames{} }

func (s sessionNames) lookup(id uint64) string {
	if name, ok := s[id]; ok {
		return name
	}
	filepath.WalkDir("/sys/fs/cgroup", func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		var st syscall.Stat_t
		if syscall.Stat(path, &st) == nil {
			s[st.Ino] = filepath.Base(path)
		}
		return nil
	})
	return s[id]
}
