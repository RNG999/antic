package domain_test

import (
	"net/netip"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/types"
)

func TestTypeTheoryAndDomainPrimitives(t *testing.T) {
	// Zero-value Refined must not be Valid (unforgeability).
	var zeroStore domain.StorePath
	if zeroStore.Valid() {
		t.Fatal("zero-value StorePath must not be Valid")
	}

	sp, err := domain.ParseStorePath("/nix/store/0123456789abcdef0123456789abcdef-nixos-system")
	if err != nil || !sp.Valid() {
		t.Fatalf("expected valid StorePath, got %v", err)
	}

	// Typestate Indexed transition: Installed -> Candidate
	inst := types.NewIndexed[domain.StorePath, domain.InstalledState](sp)
	cand := types.TransitionIndexed[domain.StorePath, domain.InstalledState, domain.CandidateState](inst)
	if cand.Get() != sp {
		t.Fatal("typestate transition lost underlying StorePath")
	}

	// GDP Proof: HostAuthProof & WithinHomeProof
	var zeroAuth domain.HostAuthProof
	if zeroAuth.Valid() {
		t.Fatal("zero-value HostAuthProof must not be Valid")
	}
	if _, err := domain.ProveHostCID(domain.NewPeerCID(3)); err == nil {
		t.Fatal("expected non-host CID to fail ProveHostCID")
	}
	auth, err := domain.ProveHostCID(domain.NewPeerCID(proto.HostCID))
	if err != nil || !auth.Valid() {
		t.Fatalf("expected valid HostAuthProof for HostCID, got %v", err)
	}

	home, _ := domain.ParseAbsPath("/home/alice")
	sub, _ := domain.ParseAbsPath("/home/alice/work")
	outside, _ := domain.ParseAbsPath("/etc/passwd")
	if _, err := domain.ProveWithinHome(home, outside); err == nil {
		t.Fatal("expected /etc/passwd to fail ProveWithinHome")
	}
	homeProof, err := domain.ProveWithinHome(home, sub)
	if err != nil || !homeProof.Valid() {
		t.Fatalf("expected valid WithinHomeProof, got %v", err)
	}

	// NonEmpty
	if _, ok := types.ParseNonEmpty[domain.DomainName](nil); ok {
		t.Fatal("empty slice must not parse as NonEmpty")
	}
	d1, _ := domain.ParseDomainName("GitHub.com")
	d2, _ := domain.ParseDomainName("Anthropic.com")
	ne := types.NewNonEmpty(d1, d2)
	if ne.Len() != 2 || ne.Head() != d1 {
		t.Fatalf("unexpected NonEmpty: %+v", ne)
	}

	// Canonical SortedSet
	set := types.BuildSet[domain.DomainName, domain.DomainName, domain.DomainNameOrder]([]domain.DomainName{d1, d2, d1})
	if !set.Canonical() || set.Len() != 2 {
		t.Fatalf("expected canonical deduplicated SortedSet of len 2, got %d", set.Len())
	}
	all := set.All()
	if all[0] != d2 || all[1] != d1 {
		t.Fatalf("expected [anthropic.com, github.com], got %v", all)
	}

	// UsableAddr
	if _, err := domain.ParseUsableAddr(netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Fatal("loopback must not be a UsableAddr")
	}
	if _, err := domain.ParseUsableAddr(netip.MustParseAddr("93.184.216.34")); err != nil {
		t.Fatalf("expected 93.184.216.34 to be usable: %v", err)
	}
}

// TestPrintable: control characters a guest could send to the host's
// terminal are escaped; ordinary text, tabs and non-ASCII letters are not.
func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"sleep 600":              "sleep 600",
		"a\tb":                   "a\tb",
		"日本語":                    "日本語",
		"\x1b]0;pwned\x07title":  `\x1b]0;pwned\atitle`,
		"\x1b]52;c;aGVsbG8=\x07": `\x1b]52;c;aGVsbG8=\a`,
		"line\nbreak\r":          `line\nbreak\r`,
		"\u009b31m":              `\u009b31m`,
	} {
		if got := domain.Printable(in); got != want {
			t.Errorf("Printable(%q) = %q, want %q", in, got, want)
		}
	}
}
