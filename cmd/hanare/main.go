// Command hanare runs CLI tools in isolated NixOS guests (see README.md).
package main

import (
	"os"

	"github.com/RNG999/hanare/internal/host/cli"
)

func main() { os.Exit(cli.Main()) }
