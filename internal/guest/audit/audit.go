// Package audit is banken's activity log (bpf/audit.bpf.c): it writes one
// JSON object per event to a serial port that the host appends to a file,
// so neither the guest user nor guest root can rewrite what was logged.
package audit

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"github.com/RNG999/hanare/internal/guest/sd"
)

// Built from bpf/audit.bpf.c by the Nix derivation before "go build".
//
//go:embed bpf/audit.bpf.o
var object []byte

// event mirrors struct event in bpf/audit.bpf.c.
type event struct {
	Cgroup  uint64
	Type    uint32
	PID     uint32
	PPID    uint32
	UID     uint32
	EUID    uint32
	Family  uint16
	Port    [2]byte // network byte order
	ArgsLen uint32
	Flags   uint32
	Addr    [112]byte
	Comm    [16]byte
	Path    [256]byte
	Args    [512]byte
}

const truncated = 1

// Record is one line of the audit log.
type Record struct {
	Time      string   `json:"time"`
	Session   string   `json:"session,omitempty"`
	Event     string   `json:"event"`
	PID       uint32   `json:"pid,omitempty"`
	PPID      uint32   `json:"ppid,omitempty"`
	Comm      string   `json:"comm,omitempty"` // set by the process itself; untrusted
	Path      string   `json:"path,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	Dest      string   `json:"dest,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Count     uint64   `json:"count,omitempty"` // "lost": events dropped since the last report
}

// Run attaches the programs for uid and writes records to out until an
// error occurs. ready is called once everything is attached.
func Run(uid uint32, out io.Writer, ready func()) error {
	if err := sd.RequireBPFLSM(); err != nil {
		return err
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(object))
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
		if prog.Type() == ebpf.LSM {
			l, err = link.AttachLSM(link.LSMOptions{Program: prog})
		} else {
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

	enc := json.NewEncoder(out)
	enc.Encode(Record{Time: now(), Event: "start"})
	ready()
	go reportLost(coll.Maps["lost"], enc)

	sessions := newSessionNames()
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
		if r, ok := decode(&e); ok {
			r.Session = sessions.lookup(e.Cgroup)
			enc.Encode(r)
		}
	}
}

func decode(e *event) (Record, bool) {
	r := Record{Time: now(), PID: e.PID, Comm: cstr(e.Comm[:]), Truncated: e.Flags&truncated != 0}
	switch e.Type {
	case 1:
		r.Event, r.PPID, r.Path = "exec", e.PPID, cstr(e.Path[:])
		if n := int(e.ArgsLen); n > 0 && n <= len(e.Args) {
			r.Argv = strings.Split(strings.TrimRight(string(e.Args[:n]), "\x00"), "\x00")
		}
	case 2, 3:
		r.Event = map[uint32]string{2: "connect", 3: "send"}[e.Type]
		r.Dest = dest(e)
	case 4:
		r.Event, r.Path = "write-open", cstr(e.Path[:])
	default:
		return r, false
	}
	return r, true
}

func dest(e *event) string {
	port := uint16(e.Port[0])<<8 | uint16(e.Port[1])
	switch e.Family {
	case unix.AF_INET:
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(e.Addr[:4])), port).String()
	case unix.AF_INET6:
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(e.Addr[:16])).Unmap(), port).String()
	case unix.AF_VSOCK:
		p := binary.LittleEndian.Uint32(e.Addr[0:4])
		cid := binary.LittleEndian.Uint32(e.Addr[4:8])
		return fmt.Sprintf("vsock:%d:%d", cid, p)
	case unix.AF_UNIX:
		if e.Addr[0] == 0 {
			return "unix:@" + cstr(e.Addr[1:])
		}
		return "unix:" + cstr(e.Addr[:])
	}
	return fmt.Sprintf("family-%d", e.Family)
}

// reportLost logs how many events the ring buffer could not take.
func reportLost(m *ebpf.Map, enc *json.Encoder) {
	var reported uint64
	for range time.Tick(5 * time.Second) {
		var perCPU []uint64
		if err := m.Lookup(uint32(0), &perCPU); err != nil {
			continue
		}
		var total uint64
		for _, n := range perCPU {
			total += n
		}
		if total > reported {
			enc.Encode(Record{Time: now(), Event: "lost", Count: total - reported})
			reported = total
		}
	}
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// OpenSerial opens the audit serial port in raw mode, so records reach the
// host byte for byte.
func OpenSerial(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	t, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err == nil {
		t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
		t.Oflag &^= unix.OPOST
		t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		t.Cflag &^= unix.CSIZE | unix.PARENB
		t.Cflag |= unix.CS8
		unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, t)
	}
	return f, nil
}
