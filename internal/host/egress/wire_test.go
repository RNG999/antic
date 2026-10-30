package egress

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func query(id uint16, labels []string, qtype uint16) []byte {
	return buildQuery(id, question{Labels: labels, Type: qtype, Class: classIN}, false, 0)
}

func TestParseQueryValid(t *testing.T) {
	msg := query(7, []string{"API", "GitHub", "com"}, typeA)
	q, err := parseQuery(msg)
	if err != nil {
		t.Fatal(err)
	}
	if q.ID != 7 || q.Question.Name != "api.github.com" || q.Question.Type != typeA || q.EDNS {
		t.Fatalf("got %+v", q)
	}
	if q.Question.Labels[1] != "GitHub" {
		t.Fatalf("case not preserved: %v", q.Question.Labels)
	}
	e := buildQuery(8, q.Question, true, 4096)
	q2, err := parseQuery(e)
	if err != nil || !q2.EDNS || q2.UDPSize != maxUDPSize {
		t.Fatalf("edns: %+v %v", q2, err)
	}
}

func TestParseQueryRejects(t *testing.T) {
	good := query(1, []string{"github", "com"}, typeA)
	mut := func(f func(b []byte) []byte) []byte { return f(append([]byte{}, good...)) }
	cases := map[string][]byte{
		"short":    good[:5],
		"response": mut(func(b []byte) []byte { b[2] |= 0x80; return b }),
		"opcode":   mut(func(b []byte) []byte { b[2] |= 0x10; return b }),
		"qd=2":     mut(func(b []byte) []byte { b[5] = 2; return b }),
		"an=1":     mut(func(b []byte) []byte { b[7] = 1; return b }),
		"ns=1":     mut(func(b []byte) []byte { b[9] = 1; return b }),
		"trailing": append(append([]byte{}, good...), 0),
		// A label containing a dot: joined it would read "evil.github.com".
		"dot in label": query(1, []string{"evil", "github.com"}, typeA),
		"nul in label": query(1, []string{"github\x00", "com"}, typeA),
		"space":        query(1, []string{"git hub", "com"}, typeA),
		"root":         query(1, nil, typeA),
		"pointer": func() []byte {
			b := append([]byte{}, good[:headerLen]...)
			b = append(b, 0xc0, 0x0c, 0, 1, 0, 1)
			return b
		}(),
		"truncated name": good[:headerLen+3],
		"long label": func() []byte {
			b := append([]byte{}, good[:headerLen]...)
			b = append(b, 64)
			b = append(b, make([]byte, 64)...)
			return append(b, 0, 0, 1, 0, 1)
		}(),
		"bad opt": mut(func(b []byte) []byte {
			b[11] = 1
			return append(b, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0) // type 1, not OPT
		}),
	}
	for name, msg := range cases {
		if _, err := parseQuery(msg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Overlong name (> 255 bytes on the wire).
	var labels []string
	for i := 0; i < 5; i++ {
		labels = append(labels, string(make63()))
	}
	if _, err := parseQuery(query(1, labels, typeA)); err == nil {
		t.Error("overlong name accepted")
	}
}

func make63() []byte {
	b := make([]byte, 63)
	for i := range b {
		b[i] = 'a'
	}
	return b
}

func TestMatches(t *testing.T) {
	patterns := []string{"*.github.com", "npmjs.org"}
	for name, want := range map[string]bool{
		"github.com":         true, // wildcard matches apex
		"api.github.com":     true, // wildcard matches subdomain
		"a.b.github.com":     true, // wildcard matches nested subdomain
		"evilgithub.com":     false,
		"github.com.evil.io": false,
		"com":                false,
		"npmjs.org":          true,  // exact match
		"registry.npmjs.org": false, // exact match does NOT match subdomain
		"npmjs.org.evil":     false,
	} {
		if got := matches(name, patterns); got != want {
			t.Errorf("matches(%q) = %v, want %v", name, got, want)
		}
	}
}

// response builds an upstream response for q with the given answer records.
type rec struct {
	owner []byte // wire name (may contain pointers)
	typ   uint16
	rdata []byte
}

func response(id uint16, q question, flags uint16, rrs ...rec) []byte {
	b := make([]byte, headerLen)
	binary.BigEndian.PutUint16(b[0:2], id)
	binary.BigEndian.PutUint16(b[2:4], 0x8180|flags)
	binary.BigEndian.PutUint16(b[4:6], 1)
	binary.BigEndian.PutUint16(b[6:8], uint16(len(rrs)))
	b = appendName(b, q.Labels)
	b = binary.BigEndian.AppendUint16(b, q.Type)
	b = binary.BigEndian.AppendUint16(b, q.Class)
	for _, r := range rrs {
		b = append(b, r.owner...)
		b = binary.BigEndian.AppendUint16(b, r.typ)
		b = binary.BigEndian.AppendUint16(b, classIN)
		b = binary.BigEndian.AppendUint32(b, 60)
		b = binary.BigEndian.AppendUint16(b, uint16(len(r.rdata)))
		b = append(b, r.rdata...)
	}
	return b
}

func wireName(labels ...string) []byte { return appendName(nil, labels) }

func TestParseResponseChain(t *testing.T) {
	q := question{Labels: []string{"GitHub", "com"}, Name: "github.com", Type: typeA, Class: classIN}
	ptr := []byte{0xc0, 0x0c} // points at the question name
	msg := response(9, q, 0,
		rec{ptr, typeCNAME, wireName("edge", "cdn", "net")},
		rec{wireName("edge", "cdn", "net"), typeA, []byte{140, 82, 114, 6}},
		// Not in the chain: must be ignored.
		rec{wireName("evil", "example"), typeA, []byte{6, 6, 6, 6}},
	)
	r, err := parseResponse(msg, 9, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Addrs) != 1 || r.Addrs[0].Addr != netip.MustParseAddr("140.82.114.6") {
		t.Fatalf("got %+v", r.Addrs)
	}
}

func TestParseResponseRejects(t *testing.T) {
	q := question{Labels: []string{"github", "com"}, Name: "github.com", Type: typeA, Class: classIN}
	ok := response(9, q, 0, rec{[]byte{0xc0, 0x0c}, typeA, []byte{1, 2, 3, 4}})
	if _, err := parseResponse(ok, 10, q); err == nil {
		t.Error("wrong ID accepted")
	}
	other := question{Labels: []string{"evil", "com"}, Name: "evil.com", Type: typeA, Class: classIN}
	if _, err := parseResponse(response(9, other, 0), 9, q); err == nil {
		t.Error("question mismatch accepted")
	}
	// Forward / self pointer (loop).
	loop := response(9, q, 0, rec{[]byte{0xc0, 0xff}, typeA, []byte{1, 2, 3, 4}})
	if _, err := parseResponse(loop, 9, q); err == nil {
		t.Error("forward pointer accepted")
	}
	// A record with wrong length.
	bad := response(9, q, 0, rec{[]byte{0xc0, 0x0c}, typeA, []byte{1, 2, 3}})
	if _, err := parseResponse(bad, 9, q); err == nil {
		t.Error("short A accepted")
	}
	// Truncated record.
	if _, err := parseResponse(ok[:len(ok)-2], 9, q); err == nil {
		t.Error("truncated response accepted")
	}
	// Every prefix of a valid response must fail cleanly (no panic).
	for i := 0; i < len(ok); i++ {
		parseResponse(ok[:i], 9, q)
	}
}

func TestUsable(t *testing.T) {
	for s, want := range map[string]bool{
		"140.82.114.6": true, "2606:4700::1": true, "192.168.1.10": true,
		"127.0.0.1": false, "0.0.0.0": false, "169.254.169.254": false,
		"::1": false, "fe80::1": false, "::ffff:1.2.3.4": false, "224.0.0.1": false,
	} {
		if got := usable(netip.MustParseAddr(s)); got != want {
			t.Errorf("usable(%s) = %v", s, got)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(query(1, []string{"github", "com"}, typeA))
	f.Fuzz(func(t *testing.T, b []byte) {
		if q, err := parseQuery(b); err == nil {
			for _, l := range q.Question.Labels {
				if !validLabel([]byte(l)) {
					t.Fatalf("invalid label accepted: %q", l)
				}
			}
			parseResponse(b, q.ID, q.Question)
		}
	})
}
