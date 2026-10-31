package domain

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"strconv"
	"strings"

	"github.com/RNG999/antic/internal/types"
)

// GuestNamePred refines string to valid guest names: ^[a-z0-9][a-z0-9-]{0,31}$.
type GuestNamePred struct{}

// Check validates a guest name.
func (GuestNamePred) Check(s string) error {
	if len(s) == 0 || len(s) > 32 {
		return fmt.Errorf("invalid guest name %q (a-z, 0-9 and -, up to 32 characters)", s)
	}
	for i, c := range s {
		if i == 0 && c == '-' {
			return fmt.Errorf("invalid guest name %q (a-z, 0-9 and -, up to 32 characters)", s)
		}
		if !isLowerAlnum(c) && c != '-' {
			return fmt.Errorf("invalid guest name %q (a-z, 0-9 and -, up to 32 characters)", s)
		}
	}
	return nil
}

// GuestName is the refinement type { s : string | GuestNamePred(s) }.
type GuestName = types.Refined[string, GuestNamePred]

// ParseGuestName validates and constructs a GuestName.
func ParseGuestName(s string) (GuestName, error) {
	return types.Parse[string, GuestNamePred](s)
}

// StorePathPred refines string to top-level Nix store paths: ^/nix/store/[0-9a-z]{32}-[^/]+$.
type StorePathPred struct{}

// Check validates a Nix store path.
func (StorePathPred) Check(s string) error {
	const prefix = "/nix/store/"
	if !strings.HasPrefix(s, prefix) {
		return fmt.Errorf("invalid system path %q", s)
	}
	rest := s[len(prefix):]
	if len(rest) < 34 || rest[32] != '-' {
		return fmt.Errorf("invalid system path %q", s)
	}
	for _, c := range rest[:32] {
		if !isLowerAlnum(c) {
			return fmt.Errorf("invalid system path %q", s)
		}
	}
	name := rest[33:]
	if len(name) == 0 || strings.Contains(name, "/") {
		return fmt.Errorf("invalid system path %q", s)
	}
	return nil
}

// StorePath is the refinement type { s : string | StorePathPred(s) }.
type StorePath = types.Refined[string, StorePathPred]

// ParseStorePath validates and constructs a StorePath.
func ParseStorePath(s string) (StorePath, error) {
	return types.Parse[string, StorePathPred](s)
}

// ShareTagPred refines string to virtiofs share tags: ^s-[0-9a-f]{12}$.
type ShareTagPred struct{}

// Check validates a virtiofs share tag.
func (ShareTagPred) Check(s string) error {
	if len(s) != 14 || !strings.HasPrefix(s, "s-") {
		return fmt.Errorf("invalid share tag %q", s)
	}
	for _, c := range s[2:] {
		if !isLowerHex(c) {
			return fmt.Errorf("invalid share tag %q", s)
		}
	}
	return nil
}

// ShareTag is the refinement type { s : string | ShareTagPred(s) }.
type ShareTag = types.Refined[string, ShareTagPred]

// ParseShareTag validates and constructs a ShareTag.
func ParseShareTag(s string) (ShareTag, error) {
	return types.Parse[string, ShareTagPred](s)
}

// AbsPathPred refines string to canonical absolute paths strictly below root ("/").
type AbsPathPred struct{}

// Check validates a canonical absolute path.
func (AbsPathPred) Check(s string) error {
	if s == "" || s == "/" || !strings.HasPrefix(s, "/") || path.Clean(s) != s {
		return fmt.Errorf("invalid canonical absolute path %q", s)
	}
	return nil
}

// AbsPath is the refinement type { s : string | AbsPathPred(s) }.
type AbsPath = types.Refined[string, AbsPathPred]

// ParseAbsPath validates and constructs an AbsPath.
func ParseAbsPath(s string) (AbsPath, error) {
	return types.Parse[string, AbsPathPred](s)
}

// WithinSubtree reports whether child lies strictly beneath parent.
func WithinSubtree(parent, child AbsPath) bool {
	if !parent.Valid() || !child.Valid() {
		return false
	}
	return strings.HasPrefix(child.Get(), parent.Get()+"/")
}

// PathsOverlap reports whether a and b are equal or one lies inside the other.
func PathsOverlap(a, b AbsPath) bool {
	if !a.Valid() || !b.Valid() {
		return false
	}
	return a.Get() == b.Get() || WithinSubtree(a, b) || WithinSubtree(b, a)
}

// UserNamePred refines string to valid POSIX login names without slashes.
type UserNamePred struct{}

// Check validates a user name.
func (UserNamePred) Check(s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/:\x00 \t\n") {
		return fmt.Errorf("invalid user name %q", s)
	}
	return nil
}

// UserName is the refinement type { s : string | UserNamePred(s) }.
type UserName = types.Refined[string, UserNamePred]

// ParseUserName validates and constructs a UserName.
func ParseUserName(s string) (UserName, error) {
	return types.Parse[string, UserNamePred](s)
}

// HomeDir returns the canonical /home/<user> AbsPath for a validated UserName.
func HomeDir(u UserName) (AbsPath, error) {
	if !u.Valid() {
		return AbsPath{}, errors.New("invalid UserName for HomeDir")
	}
	return ParseAbsPath("/home/" + u.Get())
}

// SessionUnitPred refines string to session systemd units: ^antic-session@[A-Za-z0-9:_.\-]+\.service$.
type SessionUnitPred struct{}

// Check validates a session unit name.
func (SessionUnitPred) Check(s string) error {
	const (
		prefix = "antic-session@"
		suffix = ".service"
	)
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return fmt.Errorf("invalid session %q", s)
	}
	inst := s[len(prefix) : len(s)-len(suffix)]
	if len(inst) == 0 {
		return fmt.Errorf("invalid session %q", s)
	}
	for _, c := range inst {
		if !isUnitChar(c, false) {
			return fmt.Errorf("invalid session %q", s)
		}
	}
	return nil
}

// SessionUnit is the refinement type { s : string | SessionUnitPred(s) }.
type SessionUnit = types.Refined[string, SessionUnitPred]

// ParseSessionUnit validates and constructs a SessionUnit.
func ParseSessionUnit(s string) (SessionUnit, error) {
	return types.Parse[string, SessionUnitPred](s)
}

// UnitNamePred refines string to systemd unit names: ^[A-Za-z0-9:_.@\\-]+$.
type UnitNamePred struct{}

// Check validates a systemd unit name.
func (UnitNamePred) Check(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("invalid unit %q", s)
	}
	for _, c := range s {
		if !isUnitChar(c, true) {
			return fmt.Errorf("invalid unit %q", s)
		}
	}
	return nil
}

// UnitName is the refinement type { s : string | UnitNamePred(s) }.
type UnitName = types.Refined[string, UnitNamePred]

// ParseUnitName validates and constructs a UnitName.
func ParseUnitName(s string) (UnitName, error) {
	return types.Parse[string, UnitNamePred](s)
}

// JournalLinesPred refines int to positive line counts: 1 <= n <= 100000.
type JournalLinesPred struct{}

// Check validates a journal line count.
func (JournalLinesPred) Check(n int) error {
	if n <= 0 || n > 100000 {
		return fmt.Errorf("invalid journal line count %d", n)
	}
	return nil
}

// JournalLines is the refinement type { n : int | 1 <= n <= 100000 }.
type JournalLines = types.Refined[int, JournalLinesPred]

// ParseJournalLines normalizes (<= 0 defaults to 200) and validates a line count.
func ParseJournalLines(n int) (JournalLines, error) {
	if n <= 0 {
		n = 200
	}
	return types.Parse[int, JournalLinesPred](n)
}

// MountFDPred refines int to non-negative file descriptors: fd >= 0.
type MountFDPred struct{}

// Check validates a file descriptor.
func (MountFDPred) Check(fd int) error {
	if fd < 0 {
		return fmt.Errorf("invalid negative file descriptor %d", fd)
	}
	return nil
}

// MountFD is the refinement type { fd : int | fd >= 0 }.
type MountFD = types.Refined[int, MountFDPred]

// ParseMountFD validates and constructs a MountFD.
func ParseMountFD(fd int) (MountFD, error) {
	return types.Parse[int, MountFDPred](fd)
}

// UsableAddrPred refines netip.Addr to routable unicast addresses (excluding
// unspecified, loopback, link-local, multicast, and IPv4-mapped IPv6 addresses,
// preventing DNS rebinding against local/metadata services).
type UsableAddrPred struct{}

// Check validates that a is a usable unicast address.
func (UsableAddrPred) Check(a netip.Addr) error {
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.Is4In6() {
		return fmt.Errorf("unusable or reserved IP address %s", a)
	}
	return nil
}

// UsableAddr is the refinement type { a : netip.Addr | UsableAddrPred(a) }.
type UsableAddr = types.Refined[netip.Addr, UsableAddrPred]

// ParseUsableAddr validates and constructs a UsableAddr.
func ParseUsableAddr(a netip.Addr) (UsableAddr, error) {
	return types.Parse[netip.Addr, UsableAddrPred](a)
}

// DomainNamePred refines string to canonical lowercase DNS domain names without trailing dot.
type DomainNamePred struct{}

// Check validates a lowercase domain name.
func (DomainNamePred) Check(s string) error {
	if len(s) == 0 || len(s) > 253 || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return fmt.Errorf("invalid domain name %q", s)
	}
	for _, c := range s {
		if !isLowerAlnum(c) && !strings.ContainsRune("-_.", c) {
			return fmt.Errorf("invalid domain name %q", s)
		}
	}
	return nil
}

// DomainName is the Quotient/Refinement type { s : string | DomainNamePred(s) }.
type DomainName = types.Refined[string, DomainNamePred]

// ParseDomainName projects s to its canonical lowercase representative and validates it.
func ParseDomainName(s string) (DomainName, error) {
	return types.Parse[string, DomainNamePred](strings.ToLower(s))
}

// DomainNameOrder orders DomainName values lexicographically for SortedSet.
type DomainNameOrder struct{}

// Key returns the DomainName itself as its unique key.
func (DomainNameOrder) Key(d DomainName) DomainName { return d }

// Less orders two DomainNames lexicographically.
func (DomainNameOrder) Less(a, b DomainName) bool { return a.Get() < b.Get() }

// DomainPatternPred refines string to domain match patterns (exact or "*.suffix").
type DomainPatternPred struct{}

// Check validates a domain match pattern.
func (DomainPatternPred) Check(s string) error {
	rest := strings.TrimPrefix(s, "*.")
	return (DomainNamePred{}).Check(rest)
}

// DomainPattern is the Quotient/Refinement type { s : string | DomainPatternPred(s) }.
type DomainPattern = types.Refined[string, DomainPatternPred]

// ParseDomainPattern normalizes and validates a domain pattern.
func ParseDomainPattern(s string) (DomainPattern, error) {
	return types.Parse[string, DomainPatternPred](strings.ToLower(s))
}

// DomainPatternOrder orders DomainPattern values lexicographically for SortedSet.
type DomainPatternOrder struct{}

// Key returns the DomainPattern itself as its unique key.
func (DomainPatternOrder) Key(p DomainPattern) DomainPattern { return p }

// Less orders two DomainPatterns lexicographically.
func (DomainPatternOrder) Less(a, b DomainPattern) bool { return a.Get() < b.Get() }

// DomainMatches reports whether domain matches pattern (exact or "*.base").
func DomainMatches(domain DomainName, pattern DomainPattern) bool {
	if !domain.Valid() || !pattern.Valid() {
		return false
	}
	d := domain.Get()
	p := pattern.Get()
	if strings.HasPrefix(p, "*.") {
		base := strings.TrimPrefix(p, "*.")
		return d == base || strings.HasSuffix(d, "."+base)
	}
	return d == p
}

// BoundedTimeoutPred refines int to bounded nftables element timeouts in seconds:
// 360 <= secs <= 86460 (5m+1m to 24h+1m).
type BoundedTimeoutPred struct{}

// Check validates an nftables timeout in seconds.
func (BoundedTimeoutPred) Check(secs int) error {
	if secs < 360 || secs > 86460 {
		return fmt.Errorf("timeout %ds out of bounds [360, 86460]", secs)
	}
	return nil
}

// BoundedTimeout is the refinement type { secs : int | 360 <= secs <= 86460 }.
type BoundedTimeout = types.Refined[int, BoundedTimeoutPred]

// TimeoutFromTTL computes the bounded nftables timeout in seconds for a DNS TTL:
// clamp(ttl, 300, 86400) + 60.
func TimeoutFromTTL(ttl uint32) BoundedTimeout {
	secs := int(ttl)
	if secs < 300 {
		secs = 300
	}
	if secs > 86400 {
		secs = 86400
	}
	b, _ := types.Parse[int, BoundedTimeoutPred](secs + 60)
	return b
}

// PackageNamePred refines string to nixpkgs attribute names: ^[A-Za-z0-9_][A-Za-z0-9_.+-]*$.
type PackageNamePred struct{}

// Check validates a nixpkgs attribute name.
func (PackageNamePred) Check(s string) error {
	if s == "" {
		return errors.New("empty package attribute")
	}
	for i, c := range s {
		word := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
		if !word && (i == 0 || (c != '.' && c != '+' && c != '-')) {
			return fmt.Errorf("invalid package attribute %q", s)
		}
	}
	return nil
}

// PackageName is the refinement type { s : string | PackageNamePred(s) }.
type PackageName = types.Refined[string, PackageNamePred]

// ParsePackageName validates and constructs a PackageName.
func ParsePackageName(s string) (PackageName, error) {
	return types.Parse[string, PackageNamePred](s)
}

// PackageNameOrder orders PackageName values lexicographically for SortedSet.
type PackageNameOrder struct{}

// Key returns the PackageName itself as its unique key.
func (PackageNameOrder) Key(p PackageName) PackageName { return p }

// Less orders two PackageNames lexicographically.
func (PackageNameOrder) Less(a, b PackageName) bool { return a.Get() < b.Get() }

// PortNumberPred refines int to TCP/UDP port numbers: 1 <= n <= 65535.
type PortNumberPred struct{}

// Check validates a port number.
func (PortNumberPred) Check(n int) error {
	if n < 1 || n > 65535 {
		return fmt.Errorf("port %d out of range 1..65535", n)
	}
	return nil
}

// PortNumber is the refinement type { n : int | 1 <= n <= 65535 }.
type PortNumber = types.Refined[int, PortNumberPred]

// ParsePortNumber validates and constructs a PortNumber.
func ParsePortNumber(n int) (PortNumber, error) {
	return types.Parse[int, PortNumberPred](n)
}

// CPUCountPred refines int to guest vCPU counts: 1 <= n <= 256.
type CPUCountPred struct{}

// Check validates a vCPU count.
func (CPUCountPred) Check(n int) error {
	if n < 1 || n > 256 {
		return errors.New("cpus must be between 1 and 256")
	}
	return nil
}

// CPUCount is the refinement type { n : int | 1 <= n <= 256 }.
type CPUCount = types.Refined[int, CPUCountPred]

// ParseCPUCount validates and constructs a CPUCount.
func ParseCPUCount(n int) (CPUCount, error) {
	return types.Parse[int, CPUCountPred](n)
}

// MemorySizePred refines int64 to guest memory sizes in bytes: at least 256 MiB.
type MemorySizePred struct{}

// Check validates a guest memory size.
func (MemorySizePred) Check(n int64) error {
	if n < 256<<20 {
		return errors.New("memory must be at least 256M")
	}
	return nil
}

// MemorySize is the refinement type { n : int64 | n >= 256 MiB }.
type MemorySize = types.Refined[int64, MemorySizePred]

// ParseMemorySize validates and constructs a MemorySize.
func ParseMemorySize(n int64) (MemorySize, error) {
	return types.Parse[int64, MemorySizePred](n)
}

// StepIndexPred refines int to positions in an operation plan: 0 <= n <= 64.
type StepIndexPred struct{}

// Check validates a plan position (64 is the position after a full plan).
func (StepIndexPred) Check(n int) error {
	if n < 0 || n > 64 {
		return fmt.Errorf("plan position %d out of range 0..64", n)
	}
	return nil
}

// StepIndex is the refinement type { n : int | 0 <= n <= 64 }.
type StepIndex = types.Refined[int, StepIndexPred]

// ParseStepIndex validates and constructs a StepIndex.
func ParseStepIndex(n int) (StepIndex, error) {
	return types.Parse[int, StepIndexPred](n)
}

// StepIndexOrder orders StepIndex values numerically for SortedSet.
type StepIndexOrder struct{}

// Key returns the StepIndex itself as its unique key.
func (StepIndexOrder) Key(i StepIndex) StepIndex { return i }

// Less orders two StepIndex values numerically.
func (StepIndexOrder) Less(a, b StepIndex) bool { return a.Get() < b.Get() }

// AbsPathOrder orders AbsPath values lexicographically for SortedSet.
type AbsPathOrder struct{}

// Key returns the AbsPath itself as its unique key.
func (AbsPathOrder) Key(p AbsPath) AbsPath { return p }

// Less orders two AbsPaths lexicographically.
func (AbsPathOrder) Less(a, b AbsPath) bool { return a.Get() < b.Get() }

func isUnitChar(c rune, allowAtAndSlash bool) bool {
	if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
		return true
	}
	switch c {
	case ':', '_', '.', '-':
		return true
	case '@', '\\':
		return allowAtAndSlash
	default:
		return false
	}
}

// isLowerAlnum reports whether c is a lowercase ASCII letter or a digit.
func isLowerAlnum(c rune) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }

// isLowerHex reports whether c is a lowercase hexadecimal digit.
func isLowerHex(c rune) bool { return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') }

// Printable returns s with its control characters escaped (as Go quotes
// them), for text a guest supplies that the host prints to a terminal: a
// guest must not send it escape sequences (titles, clipboard writes, ...).
func Printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\t' || !isControl(r) {
			b.WriteRune(r)
			continue
		}
		q := strconv.QuoteRune(r)
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

// isControl reports whether r is a control character (Unicode category Cc).
func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }
