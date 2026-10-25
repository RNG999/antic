package nixbuild

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/RNG999/antic"
)

// WriteBuiltinFlake writes the embedded guest flake (configured for user and
// packages) to a temporary directory and returns its path and cleanup function.
func WriteBuiltinFlake(user string, packages []string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "antic-guest-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := WriteBuiltinFlakeTo(dir, user, packages); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// WriteBuiltinFlakeTo writes the embedded guest flake (configured for user and
// packages) into dir.
func WriteBuiltinFlakeTo(dir, user string, packages []string) error {
	sys, err := nixSystem()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	err = fs.WalkDir(antic.GuestFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		dst := filepath.Join(dir, path)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		b, err := antic.GuestFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		return err
	}

	var pkgsExpr strings.Builder
	for _, p := range packages {
		pkgsExpr.WriteString("            ")
		pkgsExpr.WriteString(p)
		pkgsExpr.WriteByte('\n')
	}
	flakeNix := fmt.Sprintf(`{
  description = "antic built-in guest configuration";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  outputs = { nixpkgs, ... }: {
    nixosConfigurations.antic = nixpkgs.lib.nixosSystem {
      system = %q;
      modules = [
        ./nix/module.nix
        ({ pkgs, ... }: {
          antic.user = %q;
          environment.systemPackages = with pkgs; [
%s          ];
        })
      ];
    };
  };
}
`, sys, user, pkgsExpr.String())

	return os.WriteFile(filepath.Join(dir, "flake.nix"), []byte(flakeNix), 0o600)
}

func nixSystem() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-linux", nil
	case "arm64":
		return "aarch64-linux", nil
	default:
		return "", fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
}
