// Package agent is antic-agent's root daemon in the guest. It listens on vsock
// port proto.PortAgent and serves requests from the host only (peer CID
// proto.HostCID): mounting hot-plugged shares, switching the system to a
// host-built configuration, and listing or stopping sessions. The guest user
// has no way to ask it for anything.
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/RNG999/antic/internal/guest/sd"
	"github.com/RNG999/antic/internal/proto"
)

// Agent serves the host's requests.
type Agent struct {
	user     *user.User
	uid, gid int
	home     string

	mu     sync.Mutex // serializes mount, unmount and apply
	mounts map[string]*mount
}

// New returns an agent acting for the guest login user.
func New(userName string) (*Agent, error) {
	u, err := user.Lookup(userName)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &Agent{user: u, uid: uid, gid: gid, home: u.HomeDir, mounts: map[string]*mount{}}, nil
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
	sd.NotifyReady()
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
		if vm, ok := sa.(*unix.SockaddrVM); !ok || vm.CID != proto.HostCID {
			log.Printf("refused agent connection from %v", sa)
			c.Close()
			continue
		}
		go a.handle(c)
	}
}

// handle serves one request from the host.
func (a *Agent) handle(c *os.File) {
	defer c.Close()
	var req proto.Request
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		return
	}
	enc := json.NewEncoder(c)
	send := func(r proto.Response) { enc.Encode(r) }
	if err := a.dispatch(req, send); err != nil {
		log.Printf("%s: %v", req.Op, err)
		send(proto.Response{Error: err.Error(), Done: true})
		return
	}
	send(proto.Response{Done: true})
}

func (a *Agent) dispatch(req proto.Request, send func(proto.Response)) error {
	switch req.Op {
	case proto.OpHello:
		ready := exec.Command("systemctl", "is-active", "--quiet", "antic-ready.target").Run() == nil
		send(proto.Response{Hello: &proto.Hello{Protocol: proto.Version, User: a.user.Username, Ready: ready}})
		return nil
	case proto.OpMount:
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.mount(req.Tag, req.Path, req.ReadOnly)
	case proto.OpUnmount:
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.unmount(req.Tag)
	case proto.OpApply:
		a.mu.Lock()
		defer a.mu.Unlock()
		return apply(req.Toplevel, func(line string) { send(proto.Response{Line: line}) })
	case proto.OpSessions:
		s, err := sessions()
		if err != nil {
			return err
		}
		send(proto.Response{Sessions: s})
		return nil
	case proto.OpKill:
		return kill(req.Session)
	case proto.OpJournal:
		return journal(req.Units, req.Lines, func(line string) { send(proto.Response{Line: line}) })
	}
	return fmt.Errorf("unknown op %q", req.Op)
}
