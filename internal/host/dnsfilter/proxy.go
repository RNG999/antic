package dnsfilter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Mark is the socket mark of the filter's upstream sockets. The egress filter
// lets only marked sockets reach the upstream resolver, so passt (and thus
// the guest) cannot query it directly and bypass the allow list. Setting a
// mark needs CAP_NET_ADMIN in the namespace, which passt drops.
const Mark = 0x616e7463 // "antc"

// Listen is where the filter serves passt (the guest's DNS address is
// forwarded here).
var Listen = netip.MustParseAddrPort("127.0.0.1:53")

// nftables objects the filter adds addresses to (see guest's nftRules).
const (
	Table = "inet antic"
	Set4  = "dyn4"
	Set6  = "dyn6"
)

const (
	minTimeout   = 5 * time.Minute
	maxTimeout   = 24 * time.Hour
	queryTimeout = 8 * time.Second
	maxInflight  = 64
	maxTCPConns  = 16
)

// Config configures a Proxy.
type Config struct {
	Upstream netip.AddrPort // the namespace's resolver address (pasta forwards it)
	Domains  []string       // allowed domains (lowercase, no trailing dot)
	Nft      string         // nft binary
	Log      io.Writer
}

// Proxy is a filtering DNS forwarder.
type Proxy struct {
	cfg Config
	udp *net.UDPConn
	tcp *net.TCPListener

	nftMu sync.Mutex
	known map[netip.Addr]time.Time // when each set element expires

	logMu  sync.Mutex
	logged map[string]bool
}

// New binds the proxy's UDP and TCP sockets on Listen.
func New(cfg Config) (*Proxy, error) {
	udp, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(Listen))
	if err != nil {
		return nil, err
	}
	tcp, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(Listen))
	if err != nil {
		udp.Close()
		return nil, err
	}
	return &Proxy{cfg: cfg, udp: udp, tcp: tcp, known: map[netip.Addr]time.Time{}, logged: map[string]bool{}}, nil
}

// Serve answers queries until ctx is done or a socket fails.
func (p *Proxy) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		p.udp.Close()
		p.tcp.Close()
	}()
	errc := make(chan error, 2)
	go func() { errc <- p.serveUDP(ctx) }()
	go func() { errc <- p.serveTCP(ctx) }()
	err := <-errc
	p.udp.Close()
	p.tcp.Close()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (p *Proxy) serveUDP(ctx context.Context) error {
	sem := make(chan struct{}, maxInflight)
	buf := make([]byte, 4096)
	for {
		n, from, err := p.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		msg := bytes.Clone(buf[:n])
		select {
		case sem <- struct{}{}:
		default:
			continue // overloaded: drop, the client retries
		}
		go func() {
			defer func() { <-sem }()
			if out := p.handle(ctx, msg, false); out != nil {
				p.udp.WriteToUDPAddrPort(out, from)
			}
		}()
	}
}

func (p *Proxy) serveTCP(ctx context.Context) error {
	sem := make(chan struct{}, maxTCPConns)
	for {
		c, err := p.tcp.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-sem }()
			defer c.Close()
			for i := 0; i < 16; i++ {
				c.SetDeadline(time.Now().Add(10 * time.Second))
				msg, err := readTCPMsg(c)
				if err != nil {
					return
				}
				out := p.handle(ctx, msg, true)
				if out == nil || writeTCPMsg(c, out) != nil {
					return
				}
			}
		}()
	}
}

func readTCPMsg(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(l[:]))
	if n < headerLen {
		return nil, errMalformed
	}
	msg := make([]byte, n)
	_, err := io.ReadFull(r, msg)
	return msg, err
}

func writeTCPMsg(w io.Writer, msg []byte) error {
	if len(msg) > 0xffff {
		return errMalformed
	}
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(msg))), msg...))
	return err
}

// handle answers one client message. Allowed A/AAAA queries are forwarded;
// the answer's addresses are added to the nftables sets before the reply is
// returned, and any failure on the way is SERVFAIL (fail closed).
func (p *Proxy) handle(ctx context.Context, msg []byte, tcp bool) []byte {
	q, err := ParseQuery(msg)
	if err != nil {
		return BuildFormErr(msg)
	}
	qq := q.Question
	if qq.Class != ClassIN || !Matches(qq.Name, p.cfg.Domains) {
		p.logRefused(qq.Name)
		return BuildError(q.ID, qq, RcodeRefused)
	}
	if qq.Type != TypeA && qq.Type != TypeAAAA {
		return BuildError(q.ID, qq, 0) // NODATA: only addresses are looked up
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	id, err := randomID()
	if err != nil {
		return BuildError(q.ID, qq, RcodeServFail)
	}
	edns := q.EDNS && !tcp
	req := BuildQuery(id, qq, edns, q.UDPSize)
	var resp []byte
	if tcp {
		resp, err = p.exchangeTCP(ctx, req)
	} else {
		resp, err = p.exchangeUDP(ctx, req, id)
	}
	if err != nil {
		p.logf("dns: %s: %v", qq.Name, err)
		return BuildError(q.ID, qq, RcodeServFail)
	}
	r, err := ParseResponse(resp, id, qq)
	if err != nil {
		p.logf("dns: %s: %v", qq.Name, err)
		return BuildError(q.ID, qq, RcodeServFail)
	}
	if r.Truncated {
		if tcp {
			return BuildError(q.ID, qq, RcodeServFail)
		}
		return Truncated(q.ID, qq)
	}
	if err := p.allow(ctx, r.Addrs); err != nil {
		p.logf("dns: %s: allowing its addresses: %v", qq.Name, err)
		return BuildError(q.ID, qq, RcodeServFail)
	}
	out := bytes.Clone(resp)
	binary.BigEndian.PutUint16(out[0:2], q.ID)
	if !tcp && len(out) > q.UDPSize {
		return Truncated(q.ID, qq)
	}
	return out
}

func randomID() (uint16, error) {
	var b [2]byte
	_, err := rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:]), err
}

// markedDialer dials from sockets carrying Mark.
func markedDialer() *net.Dialer {
	return &net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, Mark)
		}); err != nil {
			return err
		}
		return serr
	}}
}

func (p *Proxy) exchangeUDP(ctx context.Context, req []byte, id uint16) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		c, err := markedDialer().DialContext(ctx, "udp", p.cfg.Upstream.String())
		if err != nil {
			return nil, err
		}
		deadline := time.Now().Add(3 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		c.SetDeadline(deadline)
		if _, err := c.Write(req); err != nil {
			c.Close()
			return nil, err
		}
		buf := make([]byte, 65535)
		for {
			n, err := c.Read(buf)
			if err != nil {
				last = err
				break
			}
			if n >= 2 && binary.BigEndian.Uint16(buf[0:2]) == id {
				c.Close()
				return bytes.Clone(buf[:n]), nil
			}
		}
		c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func (p *Proxy) exchangeTCP(ctx context.Context, req []byte) ([]byte, error) {
	c, err := markedDialer().DialContext(ctx, "tcp", p.cfg.Upstream.String())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if d, ok := ctx.Deadline(); ok {
		c.SetDeadline(d)
	}
	if err := writeTCPMsg(c, req); err != nil {
		return nil, err
	}
	return readTCPMsg(c)
}

// usable reports whether an answer address may be added to the sets at all.
// The egress filter rejects loopback, link-local, host and (in "internet"
// mode) LAN addresses before it looks at the sets; this is a second line.
func usable(a netip.Addr) bool {
	return a.IsValid() && !a.Is4In6() && !a.IsUnspecified() && !a.IsLoopback() &&
		!a.IsMulticast() && !a.IsLinkLocalUnicast() && !a.IsLinkLocalMulticast() &&
		!a.IsInterfaceLocalMulticast()
}

func timeoutFor(ttl uint32) time.Duration {
	t := time.Duration(ttl) * time.Second
	return min(max(t, minTimeout), maxTimeout) + time.Minute
}

// allow adds addresses to the nftables sets (or refreshes their timeouts),
// in one atomic nft transaction.
func (p *Proxy) allow(ctx context.Context, addrs []Answer) error {
	p.nftMu.Lock()
	defer p.nftMu.Unlock()
	now := time.Now()
	var script strings.Builder
	type upd struct {
		a   netip.Addr
		exp time.Time
	}
	var upds []upd
	for _, ans := range addrs {
		a := ans.Addr
		if !usable(a) {
			continue
		}
		t := timeoutFor(ans.TTL)
		if exp, ok := p.known[a]; ok && exp.Sub(now) > t/2 {
			continue
		}
		set := Set4
		if a.Is6() {
			set = Set6
		}
		secs := int(t / time.Second)
		// add (no-op if present), delete, add: refreshes the timeout of an
		// existing element without a moment where it is missing.
		fmt.Fprintf(&script, "add element %s %s { %s timeout %ds }\n", Table, set, a, secs)
		fmt.Fprintf(&script, "delete element %s %s { %s }\n", Table, set, a)
		fmt.Fprintf(&script, "add element %s %s { %s timeout %ds }\n", Table, set, a, secs)
		upds = append(upds, upd{a, now.Add(t)})
	}
	if len(upds) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, p.cfg.Nft, "-f", "-")
	cmd.Stdin = strings.NewReader(script.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(string(out)))
	}
	for _, u := range upds {
		p.known[u.a] = u.exp
	}
	if len(p.known) > 4096 {
		for a, exp := range p.known {
			if exp.Before(now) {
				delete(p.known, a)
			}
		}
	}
	return nil
}

func (p *Proxy) logRefused(name string) {
	p.logMu.Lock()
	first := !p.logged[name] && len(p.logged) < 1024
	if first {
		p.logged[name] = true
	}
	p.logMu.Unlock()
	if first {
		p.logf("dns: refused %s (not in allow)", name)
	}
}

func (p *Proxy) logf(format string, args ...any) {
	if p.cfg.Log != nil {
		fmt.Fprintf(p.cfg.Log, format+"\n", args...)
	}
}

// HelperCmd is the hidden argv[1] that runs antic as the first process in a
// guest's network namespace (egress filter, DNS filter, then passt).
const HelperCmd = "__netns"
