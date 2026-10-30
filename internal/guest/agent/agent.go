// Package agent is antic-agent's root daemon in the guest. It listens on vsock
// port proto.PortAgent and serves requests from the host only (peer CID
// proto.HostCID): mounting hot-plugged shares, switching the system to a
// host-built configuration, and listing or stopping sessions. The guest user
// has no way to ask it for anything.
//
// All authorization, path validation, and state transitions are decided by the
// pure Sans-I/O & TEA core in internal/tea/control using Refinement Types
// and Ghosts of Departed Proofs from internal/domain.
package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/tea/control"
)

// Agent serves the host's requests, holding the pure TEA control.Model as
// the Single Source of Truth (SSOT) for user/home/mount state alongside open
// O_PATH mount file descriptors.
type Agent struct {
	uid, gid int

	mu    sync.Mutex // serializes state transitions, mount, unmount, and apply
	model control.Model
	files map[domain.ShareTag]*os.File
}

// New returns an agent acting for the guest login user.
func New(userName string) (*Agent, error) {
	u, err := user.Lookup(userName)
	if err != nil {
		return nil, err
	}
	rUser, err := domain.ParseUserName(u.Username)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &Agent{
		uid:   uid,
		gid:   gid,
		model: control.Init(rUser),
		files: map[domain.ShareTag]*os.File{},
	}, nil
}

// Serve accepts connections on the agent port until an error occurs.
func (a *Agent) Serve() error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: proto.PortAgent}); err != nil {
		return fmt.Errorf("bind vsock port %d: %w", proto.PortAgent, err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		return err
	}
	notifyReady()
	for {
		cfd, sa, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if err == unix.EINTR || err == unix.ECONNABORTED {
				continue
			}
			return err
		}
		c := os.NewFile(uintptr(cfd), "agent-conn")
		// The guest kernel sets the peer CID, so guest processes (CID 1 via
		// loopback, or the guest's own CID) cannot pretend to be the host.
		vm, ok := sa.(*unix.SockaddrVM)
		if !ok || vm.CID != proto.HostCID {
			log.Printf("refused agent connection from %v", sa)
			c.Close()
			continue
		}
		go a.handle(c, vm.CID)
	}
}

// handle serves one request from the host.
func (a *Agent) handle(c *os.File, peerCID uint32) {
	defer c.Close()
	var req proto.Request
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		return
	}
	enc := json.NewEncoder(c)
	send := func(r proto.Response) { enc.Encode(r) }
	if err := a.dispatch(peerCID, req, send); err != nil {
		log.Printf("%s: %v", req.Op, err)
		send(proto.Response{Error: err.Error(), Done: true})
		return
	}
	send(proto.Response{Done: true})
}

func (a *Agent) dispatch(peerCID uint32, req proto.Request, send func(proto.Response)) error {
	needLock := req.Op == proto.OpMount || req.Op == proto.OpUnmount || req.Op == proto.OpApply
	if needLock {
		a.mu.Lock()
		defer a.mu.Unlock()
	}

	var inMountinfo bool
	if req.Op == proto.OpMount {
		mounted, err := tagMounted(req.Tag)
		inMountinfo = err != nil || mounted
	}

	msg := control.ParseRequest(peerCID, req, inMountinfo)
	if !needLock {
		a.mu.Lock()
	}
	next, effects := control.Update(a.model, msg)
	a.model = next
	if !needLock {
		a.mu.Unlock()
	}

	for _, eff := range effects {
		switch e := eff.(type) {
		case control.EffectReject:
			return errors.New(e.Reason.Get())

		case control.EffectReplyHello:
			ready := exec.Command("systemctl", "is-active", "--quiet", "antic-ready.target").Run() == nil
			send(proto.Response{Hello: &proto.Hello{Protocol: proto.Version, User: e.User.Get(), Ready: ready}})

		case control.EffectMountVirtiofs:
			if err := a.mount(e.Tag, e.Path, e.ReadOnly); err != nil {
				return err
			}

		case control.EffectUnmountVirtiofs:
			if err := a.unmount(e.Tag, e.Path, e.FD); err != nil {
				return err
			}

		case control.EffectExecSwitch:
			if err := apply(e.Toplevel, func(line string) { send(proto.Response{Line: line}) }); err != nil {
				return err
			}

		case control.EffectListSessions:
			s, err := sessions()
			if err != nil {
				return err
			}
			send(proto.Response{Sessions: s})

		case control.EffectStopSession:
			if err := kill(e.Unit); err != nil {
				return err
			}

		case control.EffectStreamJournal:
			if err := journal(e.Units, e.Lines, func(line string) { send(proto.Response{Line: line}) }); err != nil {
				return err
			}
		}
	}
	return nil
}

// notifyReady tells systemd (Type=notify) that antic-agent is ready to accept vsock connections.
func notifyReady() {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	if c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"}); err == nil {
		_, _ = c.Write([]byte("READY=1"))
		_ = c.Close()
	}
}

