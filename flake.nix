{
  description = "hanare: NixOS VM sandbox for CLI tools";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { nixpkgs, ... }:
  let
    pkgs = nixpkgs.legacyPackages.x86_64-linux;
  in {
    # Guest side; import from your guest flake (see examples/guest).
    nixosModules.default = ./nix/guest.nix;

    # Host-side CLI (static); "hanare update" builds and installs it to ~/.local/bin.
    packages.x86_64-linux.default = pkgs.buildGoModule {
      pname = "hanare";
      version = "0.2.0";
      src = ./cmd/hanare;
      vendorHash = "sha256-OITq/wJbuNZzgwsqFYqKmeK8cC7WliMAFZEpkbpdO7g=";
      env.CGO_ENABLED = 0;
      ldflags = [ "-s" "-w" ];
    };

    # Static vhost-user vsock backend run by hanare on the host. Its tests need
    # AF_VSOCK, which the build sandbox lacks.
    packages.x86_64-linux.vhost-device-vsock =
      pkgs.pkgsStatic.vhost-device-vsock.overrideAttrs { doCheck = false; };
  };
}
