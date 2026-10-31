package spec

import "github.com/RNG999/antic/internal/types"

// Cmd is one antic CLI invocation, with its arguments as the user typed them
// (unvalidated: Post decides what invalid input does).
//
// A Target of "" means the NAME argument was omitted, which selects the only
// defined guest if there is exactly one.
type Cmd interface {
	isCmd()
}

// CmdCreate is "antic guest create NAME [-p PKG]... [--cpus N] [--memory SIZE]
// [--network MODE] [--port PORT]...".
type CmdCreate struct {
	Name     string
	Packages []string
	CPUs     types.Option[int]
	Memory   types.Option[string]
	Network  types.Option[string]
	Ports    []string
}

// CmdApply is "antic guest apply [NAME]".
type CmdApply struct{ Target string }

// CmdInstall is "antic guest install [NAME] PKG...".
type CmdInstall struct {
	Target   string
	Packages []string
}

// CmdUninstall is "antic guest uninstall [NAME] PKG...".
type CmdUninstall struct {
	Target   string
	Packages []string
}

// CmdSet is "antic guest set [NAME] [--cpus N] [--memory SIZE] [--network MODE]".
type CmdSet struct {
	Target  string
	CPUs    types.Option[int]
	Memory  types.Option[string]
	Network types.Option[string]
}

// CmdForward is "antic guest forward [NAME] PORT... [-p PROTO]".
type CmdForward struct {
	Target string
	Ports  []string
	Proto  string
}

// CmdUnforward is "antic guest unforward [NAME] PORT...".
type CmdUnforward struct {
	Target string
	Ports  []string
}

// CmdStart is "antic guest start [NAME...]".
type CmdStart struct{ Names []string }

// CmdStop is "antic guest stop [NAME...]".
type CmdStop struct{ Names []string }

// CmdRestart is "antic guest restart [NAME...]".
type CmdRestart struct{ Names []string }

// CmdRm is "antic guest rm [-y] NAME..." run without a terminal.
type CmdRm struct {
	Names []string
	Yes   bool
}

// CmdMount is "antic guest mount [NAME] [--ro] DIR...".
type CmdMount struct {
	Target   string
	Dirs     []string
	ReadOnly bool
}

// CmdUnmount is "antic guest unmount [NAME] DIR...".
type CmdUnmount struct {
	Target string
	Dirs   []string
}

// CmdEdit is "antic guest edit [NAME]" with an EDITOR that replaces the file
// with New.
type CmdEdit struct {
	Target string
	New    RawConfig
}

// CmdRun is "antic run [NAME] -- CMD" where CMD exits with Code.
type CmdRun struct {
	Target string
	Code   int
}

// CmdSpawn is "antic run NAME -T -- sh -c COMMAND" left running: a session
// that runs until it is killed (or the guest stops).
type CmdSpawn struct {
	Target  string
	Command string
}

// CmdKill is "antic guest kill NAME SESSION...", naming each session by
// its command line (the test finds its unit with "guest ps").
type CmdKill struct {
	Target   string
	Commands []string
}

func (CmdSpawn) isCmd()     {}
func (CmdKill) isCmd()      {}
func (CmdCreate) isCmd()    {}
func (CmdApply) isCmd()     {}
func (CmdInstall) isCmd()   {}
func (CmdUninstall) isCmd() {}
func (CmdSet) isCmd()       {}
func (CmdForward) isCmd()   {}
func (CmdUnforward) isCmd() {}
func (CmdStart) isCmd()     {}
func (CmdStop) isCmd()      {}
func (CmdRestart) isCmd()   {}
func (CmdRm) isCmd()        {}
func (CmdMount) isCmd()     {}
func (CmdUnmount) isCmd()   {}
func (CmdEdit) isCmd()      {}
func (CmdRun) isCmd()       {}
