// Package guest implements the "hanare vm" commands, which run inside the
// guest VM as root, started by the systemd units in module.nix. They load
// hanare's BPF programs (bpf/).
package guest

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

const usage = `usage: hanare vm <command>

  restrict-userns     deny creating user namespaces to everyone but root
  unrestrict-userns   remove that restriction
  audit [-uid N]      log program executions, connections and files opened
                      for writing by user N (default 1000) as JSON lines
`

// Main runs "hanare vm ARGS..." and returns the exit status.
func Main(args []string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "restrict-userns":
		err = restrictUserns()
	case "unrestrict-userns":
		err = os.RemoveAll(usernsPin)
	case "audit":
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		uid := fs.Uint("uid", 1000, "user to audit")
		fs.Parse(args[1:])
		err = audit(uint32(*uid))
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare vm: %v\n", err)
		return 1
	}
	return 0
}

// requireBPFLSM fails unless "bpf" is an active LSM (lsm= boot parameter).
func requireBPFLSM() error {
	b, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return err
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), ",") {
		if l == "bpf" {
			return nil
		}
	}
	return errors.New("BPF LSM is not active (add bpf to lsm=)")
}
