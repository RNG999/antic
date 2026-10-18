// Package proto is the contract between the host (hanare) and the guest
// (banken and the NixOS module in nix/module.nix): ports, credential names,
// paths and the agent protocol. Both sides change together; Version is
// checked when the host connects to a guest.
package proto

// Version of this contract. Bump on any incompatible change; the host
// refuses to talk to a guest built with a different version.
const Version = 1

// vsock ports in the guest.
const (
	PortSession        = 22 // sshd: "hanare run", "hanare shell"
	PortSessionOffline = 23 // sshd in a unit without network
	PortAgent          = 24 // banken agent; accepts connections from the host (CID 2) only
)

// HostCID is the vsock address of the host as seen from the guest.
const HostCID = 2

// systemd credentials the host passes at boot (SMBIOS OEM strings).
const (
	CredAuthorizedKeys = "hanare.ssh.authorized_keys" // host's public key for the guest user
	CredHostKey        = "hanare.ssh.host_key"        // guest sshd host key, generated and pinned by the host
	CredProtocol       = "hanare.protocol"            // Version, as decimal
	CredMounts         = "fstab.extra"                // configured shares (read by systemd-fstab-generator)
)

// Guest paths.
const (
	StoreTag  = "hanare-store" // virtiofs tag of the read-only /nix
	RunDir    = "/run/hanare"  // root-owned runtime state in the guest
	AuditPort = "/dev/ttyS0"   // serial port the host writes to an append-only file
)

// Share tags are "s-" plus 12 hex digits (derived from the host path).
const ShareTagPattern = `^s-[0-9a-f]{12}$`

// ProtectedNames are paths inside shares that host-side tools execute or
// trust as configuration (git, AI agents, editors, direnv, mise, ...). The
// host exports existing ones read-only.
var ProtectedNames = []string{
	".git", ".claude", ".codex", ".gemini", ".cursor", ".vscode", ".idea",
	".devcontainer", ".husky", ".githooks", ".mise", ".mcp.json", "CLAUDE.md",
	"CLAUDE.local.md", "AGENTS.md", "GEMINI.md", ".cursorrules", ".envrc",
	"mise.toml", ".mise.toml",
}

// Agent protocol: one JSON Request per connection, answered by a stream of
// JSON Responses (one per line); the last one has Done set.
type Request struct {
	Op string `json:"op"` // see Op* constants

	// mount / unmount
	Tag      string `json:"tag,omitempty"`
	Path     string `json:"path,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`

	// apply
	Toplevel string `json:"toplevel,omitempty"`

	// kill
	Session string `json:"session,omitempty"`

	// journal: units to show (empty: all), number of lines
	Units []string `json:"units,omitempty"`
	Lines int      `json:"lines,omitempty"`
}

const (
	OpHello    = "hello"    // -> Hello
	OpMount    = "mount"    // mount virtiofs Tag at Path (created as the guest user)
	OpUnmount  = "unmount"  // unmount the mount made for Tag
	OpApply    = "apply"    // switch to Toplevel; Lines carry its output
	OpSessions = "sessions" // -> Sessions
	OpKill     = "kill"     // stop Session
	OpJournal  = "journal"  // the guest's journal (this boot); Lines carry it
)

type Response struct {
	Line     string    `json:"line,omitempty"` // progress output
	Hello    *Hello    `json:"hello,omitempty"`
	Sessions []Session `json:"sessions,omitempty"`
	Error    string    `json:"error,omitempty"`
	Done     bool      `json:"done,omitempty"`
}

type Hello struct {
	Protocol int    `json:"protocol"`
	User     string `json:"user"`  // guest login user (must equal the host user)
	Ready    bool   `json:"ready"` // banken-ready.target reached
}

// SessionSlices are the cgroup directories holding session units (systemd
// puts template instances in per-template slices; "-" is escaped as \x2d).
var SessionSlices = []string{
	"/sys/fs/cgroup/system.slice/system-banken\\x2dsession.slice",
	"/sys/fs/cgroup/system.slice/system-banken\\x2dsession\\x2doffline.slice",
}

type Session struct {
	Unit    string `json:"unit"`
	Offline bool   `json:"offline"`
	Started int64  `json:"started"` // Unix seconds
	Command string `json:"command"`
	PIDs    []int  `json:"pids"`
}
