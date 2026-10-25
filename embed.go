// Package antic embeds the files needed to build a guest OS without an
// external guest flake.
package antic

import "embed"

//go:embed flake.lock go.mod go.sum
//go:embed cmd/antic-agent
//go:embed internal/guest internal/proto
//go:embed nix/module.nix nix/packages.nix nix/proto.json
var GuestFS embed.FS
