// Command antic runs CLI tools in isolated NixOS guests (see README.md).
package main

import (
	"os"

	"github.com/RNG999/antic/internal/host/cli"
)

func main() { os.Exit(cli.Main()) }
