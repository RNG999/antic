// Package proto is the contract between the host (antic) and the guest
// (antic-agent and the NixOS module in nix/module.nix): ports, credential
// names, paths, the session protocol and the agent protocol. Both sides
// change together; Version is checked when the host connects to a guest.
package proto

// Version of this contract. Bump on any incompatible change; the host
// refuses to talk to a guest built with a different version.
const Version = 1

// vsock ports in the guest.
const (
	PortSession = 22 // antic-agent session: "antic run", "antic shell"
	PortAgent   = 24 // antic-agent root daemon; accepts connections from the host (CID 2) only
)

// HostCID is the vsock address of the host as seen from the guest.
const HostCID = 2

// systemd credentials the host passes at boot (SMBIOS OEM strings).
const (
	CredProtocol = "antic.protocol" // Version, as decimal
	CredMounts   = "fstab.extra"    // configured shares (read by systemd-fstab-generator)
)

// Guest paths.
const (
	StoreTag = "antic-store" // virtiofs tag of the read-only /nix
	RunDir   = "/run/antic"  // root-owned runtime state in the guest
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
	// antic's own project config: a guest sharing its project must not
	// rewrite how it is run (its network policy, shares or flake) (D22).
	"antic.toml",
}

// SessionReq is the JSON header sent at the start of a session connection on
// PortSession, followed by framed I/O.
type SessionReq struct {
	Argv []string `json:"argv,omitempty"` // empty: interactive login shell
	Dir  string   `json:"dir,omitempty"`  // working directory ("" or "~": home directory)
	Env  []string `json:"env,omitempty"`  // extra environment variables (e.g. TERM=...)
	PTY  bool     `json:"pty,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

// Session stream frame types ([1-byte type][4-byte big-endian length][payload]).
const (
	FrameStdin      byte = 1 // host -> guest: stdin data
	FrameCloseStdin byte = 2 // host -> guest: EOF on stdin
	FrameResize     byte = 3 // host -> guest: 4 bytes (rows uint16, cols uint16)
	FrameSignal     byte = 4 // host -> guest: 1 byte (signal number)
	FramePing       byte = 5 // host -> guest: keepalive

	FrameStdout byte = 1 // guest -> host: stdout (or pty) data
	FrameStderr byte = 2 // guest -> host: stderr data
	FrameExit   byte = 3 // guest -> host: 4 bytes (exit status)
	FrameError  byte = 4 // guest -> host: error message if setup/exec failed
	FramePong   byte = 5 // guest -> host: keepalive reply
)

// Op is the closed enum of agent protocol operations.
type Op string

// Agent protocol: one JSON Request per connection, answered by a stream of
// JSON Responses (one per line); the last one has Done set.
type Request struct {
	Op Op `json:"op"` // see Op* constants

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
	OpHello    Op = "hello"    // -> Hello
	OpMount    Op = "mount"    // mount virtiofs Tag at Path (created as the guest user)
	OpUnmount  Op = "unmount"  // unmount the mount made for Tag
	OpApply    Op = "apply"    // switch to Toplevel; Lines carry its output
	OpSessions Op = "sessions" // -> Sessions
	OpKill     Op = "kill"     // stop Session
	OpJournal  Op = "journal"  // the guest's journal (this boot); Lines carry it
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
	Ready    bool   `json:"ready"` // antic-ready.target reached
}

// SessionSlices are the cgroup directories holding session units (systemd
// puts template instances in per-template slices; "-" is escaped as \x2d).
var SessionSlices = []string{
	"/sys/fs/cgroup/system.slice/system-antic\\x2dsession.slice",
}

type Session struct {
	Unit    string `json:"unit"`
	Started int64  `json:"started"` // Unix seconds
	Command string `json:"command"`
	PIDs    []int  `json:"pids"`
}
