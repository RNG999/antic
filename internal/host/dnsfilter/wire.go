// Package dnsfilter is the DNS side of a guest's domain allow list. It runs
// inside the guest's network namespace (next to passt), answers the guest's
// DNS queries, forwards only queries for allowed names upstream, and adds the
// addresses in the answers to an nftables set before the guest sees them.
// Everything not in that set (or the static allow list) is rejected by the
// namespace's egress filter, so connecting by IP without asking DNS fails.
//
// Parsing is deliberately strict: anything unusual is refused rather than
// interpreted, since a parser bug here would be a filter bypass.
package dnsfilter

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
)

// DNS constants used here.
const (
	TypeA     = 1
	TypeCNAME = 5
	TypeAAAA  = 28
	TypeOPT   = 41
	ClassIN   = 1

	RcodeFormErr  = 1
	RcodeServFail = 2
	RcodeRefused  = 5

	headerLen  = 12
	maxNameLen = 255 // wire format, including length bytes and the root label
	minUDPSize = 512
	maxUDPSize = 1232
)

var errMalformed = errors.New("malformed DNS message")

// Question is a validated query question. Labels keep the client's case
// (resolvers may randomize it and compare); Name is the lowercase form used
// for matching.
type Question struct {
	Labels []string
	Name   string // lowercase, dot-separated, no trailing dot
	Type   uint16
	Class  uint16
}

// Query is a validated client query.
type Query struct {
	ID       uint16
	Question Question
	UDPSize  int  // the client's EDNS UDP payload size (512 without EDNS)
	EDNS     bool // whether the client sent an OPT record
}

// validLabel accepts letters, digits, '-' and '_' only: no dots, NULs,
// spaces or escapes inside a label, so the joined name means exactly what
// the wire name means.
func validLabel(b []byte) bool {
	if len(b) == 0 || len(b) > 63 {
		return false
	}
	for _, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ParseQuery validates a client query: a standard query with exactly one
// question (uncompressed, LDH/underscore labels), no answer or authority
// records, and at most an EDNS OPT record, with nothing after it.
func ParseQuery(msg []byte) (Query, error) {
	var q Query
	if len(msg) < headerLen || len(msg) > 4096 {
		return q, errMalformed
	}
	q.ID = binary.BigEndian.Uint16(msg[0:2])
	flags := binary.BigEndian.Uint16(msg[2:4])
	if flags&0x8000 != 0 || (flags>>11)&0xf != 0 { // QR=1 or opcode != QUERY
		return q, errMalformed
	}
	qd := binary.BigEndian.Uint16(msg[4:6])
	an := binary.BigEndian.Uint16(msg[6:8])
	ns := binary.BigEndian.Uint16(msg[8:10])
	ar := binary.BigEndian.Uint16(msg[10:12])
	if qd != 1 || an != 0 || ns != 0 || ar > 1 {
		return q, errMalformed
	}
	off := headerLen
	var labels []string
	var lower []string
	wire := 0
	for {
		if off >= len(msg) {
			return q, errMalformed
		}
		l := int(msg[off])
		off++
		wire++
		if l == 0 {
			break
		}
		if l > 63 { // also rejects compression pointers (0xc0) and reserved types
			return q, errMalformed
		}
		if off+l > len(msg) {
			return q, errMalformed
		}
		b := msg[off : off+l]
		if !validLabel(b) {
			return q, errMalformed
		}
		labels = append(labels, string(b))
		lower = append(lower, strings.ToLower(string(b)))
		off += l
		wire += l
		if wire > maxNameLen {
			return q, errMalformed
		}
	}
	if len(labels) == 0 { // the root
		return q, errMalformed
	}
	if off+4 > len(msg) {
		return q, errMalformed
	}
	q.Question = Question{
		Labels: labels,
		Name:   strings.Join(lower, "."),
		Type:   binary.BigEndian.Uint16(msg[off : off+2]),
		Class:  binary.BigEndian.Uint16(msg[off+2 : off+4]),
	}
	off += 4
	q.UDPSize = minUDPSize
	if ar == 1 {
		// OPT: root name, type 41, class = UDP size, TTL, RDLENGTH, RDATA.
		if off+11 > len(msg) || msg[off] != 0 || binary.BigEndian.Uint16(msg[off+1:off+3]) != TypeOPT {
			return q, errMalformed
		}
		size := int(binary.BigEndian.Uint16(msg[off+3 : off+5]))
		rdlen := int(binary.BigEndian.Uint16(msg[off+9 : off+11]))
		off += 11
		if off+rdlen > len(msg) {
			return q, errMalformed
		}
		off += rdlen
		q.EDNS = true
		q.UDPSize = min(max(size, minUDPSize), maxUDPSize)
	}
	if off != len(msg) {
		return q, errMalformed
	}
	return q, nil
}

// appendName appends labels in wire format.
func appendName(b []byte, labels []string) []byte {
	for _, l := range labels {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

// BuildQuery builds the upstream query: a fresh ID, recursion desired, the
// validated question re-encoded from its labels, and (only if the client used
// EDNS) an OPT record with no options. Nothing else from the client's message
// is forwarded.
func BuildQuery(id uint16, q Question, edns bool, udpSize int) []byte {
	b := make([]byte, headerLen, 64)
	binary.BigEndian.PutUint16(b[0:2], id)
	binary.BigEndian.PutUint16(b[2:4], 0x0100) // RD
	binary.BigEndian.PutUint16(b[4:6], 1)
	if edns {
		binary.BigEndian.PutUint16(b[10:12], 1)
	}
	b = appendName(b, q.Labels)
	b = binary.BigEndian.AppendUint16(b, q.Type)
	b = binary.BigEndian.AppendUint16(b, q.Class)
	if edns {
		b = append(b, 0)
		b = binary.BigEndian.AppendUint16(b, TypeOPT)
		b = binary.BigEndian.AppendUint16(b, uint16(udpSize))
		b = append(b, 0, 0, 0, 0, 0, 0) // extended RCODE/flags, RDLENGTH 0
	}
	return b
}

// BuildError builds a reply with rcode and the client's question (no
// records). With rcode 0 this is NODATA (NOERROR without answers).
func BuildError(id uint16, q Question, rcode int) []byte {
	b := make([]byte, headerLen, 64)
	binary.BigEndian.PutUint16(b[0:2], id)
	binary.BigEndian.PutUint16(b[2:4], 0x8180|uint16(rcode&0xf)) // QR, RD, RA
	binary.BigEndian.PutUint16(b[4:6], 1)
	b = appendName(b, q.Labels)
	b = binary.BigEndian.AppendUint16(b, q.Type)
	b = binary.BigEndian.AppendUint16(b, q.Class)
	return b
}

// BuildFormErr answers a message that could not be parsed as a query.
func BuildFormErr(msg []byte) []byte {
	if len(msg) < 2 {
		return nil
	}
	b := make([]byte, headerLen)
	copy(b[0:2], msg[0:2])
	binary.BigEndian.PutUint16(b[2:4], 0x8000|RcodeFormErr)
	return b
}

// Truncated returns a header-and-question reply with TC set, telling the
// client to retry over TCP.
func Truncated(id uint16, q Question) []byte {
	b := BuildError(id, q, 0)
	b[2] |= 0x02 // TC
	return b
}

// Answer is an address from a response, with its TTL.
type Answer struct {
	Addr netip.Addr
	TTL  uint32
}

// Response is the part of an upstream response the filter needs.
type Response struct {
	Rcode     int
	Truncated bool
	Addrs     []Answer
}

// readName reads a possibly compressed name starting at off and returns it
// lowercased and the offset after it in the original position. Pointers must
// point backwards, which also rules out loops.
func readName(msg []byte, off int) (string, int, error) {
	var parts []string
	end := -1
	wire := 0
	pos := off
	for hops := 0; ; {
		if pos >= len(msg) {
			return "", 0, errMalformed
		}
		l := int(msg[pos])
		switch {
		case l == 0:
			if end < 0 {
				end = pos + 1
			}
			return strings.Join(parts, "."), end, nil
		case l&0xc0 == 0xc0:
			if pos+1 >= len(msg) {
				return "", 0, errMalformed
			}
			ptr := int(binary.BigEndian.Uint16(msg[pos:pos+2]) & 0x3fff)
			if ptr >= pos || hops > 64 {
				return "", 0, errMalformed
			}
			if end < 0 {
				end = pos + 2
			}
			pos = ptr
			hops++
		case l > 63:
			return "", 0, errMalformed
		default:
			if pos+1+l > len(msg) {
				return "", 0, errMalformed
			}
			b := msg[pos+1 : pos+1+l]
			// Answers may legitimately carry names we would not accept in a
			// query; such names simply never match the chain.
			parts = append(parts, strings.ToLower(string(b)))
			wire += 1 + l
			if wire > maxNameLen {
				return "", 0, errMalformed
			}
			pos += 1 + l
		}
	}
}

// ParseResponse validates an upstream response to the query (id, q) and
// returns the A/AAAA addresses that answer it: records owned by the question
// name or by a name reached from it through CNAME records in the answer
// section. Authority and additional sections are ignored.
func ParseResponse(msg []byte, id uint16, q Question) (Response, error) {
	var r Response
	if len(msg) < headerLen {
		return r, errMalformed
	}
	if binary.BigEndian.Uint16(msg[0:2]) != id {
		return r, errors.New("DNS response ID mismatch")
	}
	flags := binary.BigEndian.Uint16(msg[2:4])
	if flags&0x8000 == 0 || (flags>>11)&0xf != 0 {
		return r, errMalformed
	}
	r.Rcode = int(flags & 0xf)
	r.Truncated = flags&0x0200 != 0
	qd := binary.BigEndian.Uint16(msg[4:6])
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	if qd != 1 {
		return r, errMalformed
	}
	name, off, err := readName(msg, headerLen)
	if err != nil {
		return r, err
	}
	if off+4 > len(msg) || name != q.Name ||
		binary.BigEndian.Uint16(msg[off:off+2]) != q.Type ||
		binary.BigEndian.Uint16(msg[off+2:off+4]) != q.Class {
		return r, errors.New("DNS response question mismatch")
	}
	off += 4

	type rr struct {
		owner string
		typ   uint16
		ttl   uint32
		cname string
		addr  netip.Addr
	}
	var rrs []rr
	for i := 0; i < an; i++ {
		owner, o, err := readName(msg, off)
		if err != nil {
			return r, err
		}
		if o+10 > len(msg) {
			return r, errMalformed
		}
		typ := binary.BigEndian.Uint16(msg[o : o+2])
		class := binary.BigEndian.Uint16(msg[o+2 : o+4])
		ttl := binary.BigEndian.Uint32(msg[o+4 : o+8])
		rdlen := int(binary.BigEndian.Uint16(msg[o+8 : o+10]))
		o += 10
		if o+rdlen > len(msg) {
			return r, errMalformed
		}
		rd := msg[o : o+rdlen]
		if class == ClassIN {
			switch typ {
			case TypeA:
				if rdlen != 4 {
					return r, errMalformed
				}
				rrs = append(rrs, rr{owner: owner, typ: typ, ttl: ttl, addr: netip.AddrFrom4([4]byte(rd))})
			case TypeAAAA:
				if rdlen != 16 {
					return r, errMalformed
				}
				rrs = append(rrs, rr{owner: owner, typ: typ, ttl: ttl, addr: netip.AddrFrom16([16]byte(rd))})
			case TypeCNAME:
				target, tend, err := readName(msg, o)
				if err != nil || tend != o+rdlen {
					return r, errMalformed
				}
				rrs = append(rrs, rr{owner: owner, typ: typ, ttl: ttl, cname: target})
			}
		}
		off = o + rdlen
	}

	chain := map[string]bool{q.Name: true}
	for changed := true; changed; {
		changed = false
		for _, x := range rrs {
			if x.typ == TypeCNAME && chain[x.owner] && !chain[x.cname] {
				chain[x.cname] = true
				changed = true
			}
		}
		if len(chain) > 32 {
			return r, errors.New("DNS response CNAME chain too long")
		}
	}
	for _, x := range rrs {
		if (x.typ == TypeA || x.typ == TypeAAAA) && chain[x.owner] {
			r.Addrs = append(r.Addrs, Answer{Addr: x.addr, TTL: x.ttl})
		}
	}
	return r, nil
}

// Matches reports whether name (lowercase, no trailing dot) is one of the
// allowed domains or a subdomain of one.
func Matches(name string, allowed []string) bool {
	for _, d := range allowed {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}
