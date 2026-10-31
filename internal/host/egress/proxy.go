package egress

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

	"github.com/RNG999/antic/internal/domain"
	teaegress "github.com/RNG999/antic/internal/tea/egress"
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
	queryTimeout = 8 * time.Second
	maxInflight  = 64
	maxTCPConns  = 16
)

// Config configures a Proxy.
type Config struct {
	Upstream netip.AddrPort // the namespace's resolver address (pasta forwards it)
	Allow    []string       // allowed domains (lowercase, no trailing dot)
	Deny     []string       // denied domains (lowercase, no trailing dot)
	// DefaultDeny refuses names Allow does not match (network.default = "deny").
	DefaultDeny bool
	NFT         string // nft binary
	Log         io.Writer
}

// Proxy is a filtering DNS forwarder whose domain policy, refused-domain log,
// and nftables set expiration state live in a single pure TEA teaegress.Model
// (Single Source of Truth).
type Proxy struct {
	cfg Config
	udp *net.UDPConn
	tcp *net.TCPListener

	mu     sync.Mutex
	model  teaegress.Model
	inited bool
}

func initModel(cfg Config) teaegress.Model {
	var allow, deny []domain.DomainPattern
	for _, s := range cfg.Allow {
		if p, err := domain.ParseDomainPattern(s); err == nil {
			allow = append(allow, p)
		}
	}
	for _, s := range cfg.Deny {
		if p, err := domain.ParseDomainPattern(s); err == nil {
			deny = append(deny, p)
		}
	}
	def := teaegress.DefaultAllow
	if cfg.DefaultDeny {
		def = teaegress.DefaultDeny
	}
	return teaegress.Init(def, allow, deny)
}

func (p *Proxy) ensureModel() {
	if !p.inited {
		p.model = initModel(p.cfg)
		p.inited = true
	}
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
	return &Proxy{
		cfg:    cfg,
		udp:    udp,
		tcp:    tcp,
		model:  initModel(cfg),
		inited: true,
	}, nil
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
	return stopped(ctx, err)
}

// stopped is err, unless ctx was cancelled: then the listeners were closed
// on purpose and the error only reports that.
func stopped(ctx context.Context, err error) error {
	select {
	case <-ctx.Done():
		return nil
	default:
		return err
	}
}

func (p *Proxy) serveUDP(ctx context.Context) error {
	sem := make(chan struct{}, maxInflight)
	buf := make([]byte, 4096)
	for {
		n, from, err := p.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return stopped(ctx, err)
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
			return stopped(ctx, err)
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

// domainAllowed delegates domain policy evaluation to the pure TEA teaegress.Model (SSOT).
func (p *Proxy) domainAllowed(name string) bool {
	p.mu.Lock()
	p.ensureModel()
	m := p.model
	p.mu.Unlock()
	d, err := domain.ParseDomainName(name)
	if err != nil {
		return false
	}
	return m.DomainAllowed(d)
}

func (p *Proxy) handle(ctx context.Context, msg []byte, tcp bool) []byte {
	q, err := parseQuery(msg)
	if err != nil {
		return buildFormErr(msg)
	}
	qq := q.Question
	dom, _ := domain.ParseDomainName(qq.Name)
	kind := teaegress.QueryTypeAddress
	if qq.Class != classIN {
		kind = teaegress.QueryClassNonIN
	} else if qq.Type != typeA && qq.Type != typeAAAA {
		kind = teaegress.QueryTypeOtherIN
	}

	p.mu.Lock()
	p.ensureModel()
	next, effects := teaegress.Update(p.model, teaegress.MsgEvaluateQuery{
		Domain: dom,
		Kind:   kind,
	})
	p.model = next
	p.mu.Unlock()

	for _, eff := range effects {
		switch e := eff.(type) {
		case teaegress.EffectLogRefused:
			p.logf("dns: refused %s (not in allow)", e.Domain.Get())
		case teaegress.EffectReplyImmediate:
			if e.Code == teaegress.ReplyNoData {
				return buildError(q.ID, qq, 0)
			}
			return buildError(q.ID, qq, rcodeRefused)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	id, err := randomID()
	if err != nil {
		return buildError(q.ID, qq, rcodeServFail)
	}
	edns := q.EDNS && !tcp
	req := buildQuery(id, qq, edns, q.UDPSize)
	var resp []byte
	if tcp {
		resp, err = p.exchangeTCP(ctx, req)
	} else {
		resp, err = p.exchangeUDP(ctx, req, id)
	}
	if err != nil {
		p.logf("dns: %s: %v", qq.Name, err)
		return buildError(q.ID, qq, rcodeServFail)
	}
	r, err := parseResponse(resp, id, qq)
	if err != nil {
		p.logf("dns: %s: %v", qq.Name, err)
		return buildError(q.ID, qq, rcodeServFail)
	}
	if r.Truncated {
		if tcp {
			return buildError(q.ID, qq, rcodeServFail)
		}
		return truncated(q.ID, qq)
	}
	if len(p.cfg.Allow) > 0 {
		if err := p.allow(ctx, r.Addrs); err != nil {
			p.logf("dns: %s: allowing its addresses: %v", qq.Name, err)
			return buildError(q.ID, qq, rcodeServFail)
		}
	}
	out := bytes.Clone(resp)
	binary.BigEndian.PutUint16(out[0:2], q.ID)
	if !tcp && len(out) > q.UDPSize {
		return truncated(q.ID, qq)
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

// usable reports whether an answer address satisfies the SSOT UsableAddr refinement.
func usable(a netip.Addr) bool {
	_, err := domain.ParseUsableAddr(a)
	return err == nil
}

// allow adds addresses to the nftables sets (or refreshes their timeouts),
// in one atomic nft transaction driven by pure teaegress.Update.
func (p *Proxy) allow(ctx context.Context, addrs []answer) error {
	var candidates []teaegress.AnswerCandidate
	for _, ans := range addrs {
		ua, err := domain.ParseUsableAddr(ans.Addr)
		if err != nil {
			continue
		}
		candidates = append(candidates, teaegress.AnswerCandidate{
			Addr:    ua,
			Timeout: domain.TimeoutFromTTL(ans.TTL),
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureModel()
	next, effects := teaegress.Update(p.model, teaegress.MsgRecordAnswers{
		Now:     domain.NewUnixSec(time.Now().Unix()),
		Answers: candidates,
	})
	for _, eff := range effects {
		updEff, ok := eff.(teaegress.EffectUpdateNFTSet)
		if !ok {
			continue
		}
		var script strings.Builder
		for _, u := range updEff.Updates.Slice() {
			a := u.Addr.Get()
			secs := u.Timeout.Get()
			set := Set4
			if a.Is6() {
				set = Set6
			}
			// add (no-op if present), delete, add: refreshes the timeout of an
			// existing element without a moment where it is missing.
			fmt.Fprintf(&script, "add element %s %s { %s timeout %ds }\n", Table, set, a, secs)
			fmt.Fprintf(&script, "delete element %s %s { %s }\n", Table, set, a)
			fmt.Fprintf(&script, "add element %s %s { %s timeout %ds }\n", Table, set, a, secs)
		}
		cmd := exec.CommandContext(ctx, p.cfg.NFT, "-f", "-")
		cmd.Stdin = strings.NewReader(script.String())
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	p.model = next
	return nil
}

func (p *Proxy) logf(format string, args ...any) {
	if p.cfg.Log != nil {
		fmt.Fprintf(p.cfg.Log, format+"\n", args...)
	}
}

// HelperCmd is the hidden argv[1] that runs antic as the first process in a
// guest's network namespace (egress filter, DNS filter, then passt).
const HelperCmd = "__netns"
