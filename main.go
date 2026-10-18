// Command hanare runs CLI tools in an isolated NixOS VM. Host-side commands
// live in package host; "hanare guest ..." commands, run inside the VM by its
// systemd units, live in package guest.
package main

import (
	"os"
	"path/filepath"

	"github.com/RNG999/hanare/guest"
	"github.com/RNG999/hanare/host"
)

func main() {
	// Via a tool symlink (see "hanare link"), every argument belongs to the tool.
	if filepath.Base(os.Args[0]) == "hanare" && len(os.Args) > 1 && os.Args[1] == "guest" {
		os.Exit(guest.Main(os.Args[2:]))
	}
	os.Exit(host.Main())
}
