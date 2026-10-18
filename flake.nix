{
  description = "hanare: NixOS VM sandbox for CLI tools";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { nixpkgs, ... }: {
    # Guest side; import from your guest flake (see examples/guest).
    nixosModules.default = ./nix/guest.nix;

    # Host-side CLI (static); "hanare update" builds and installs it to ~/.local/bin.
    packages.x86_64-linux.default = nixpkgs.legacyPackages.x86_64-linux.buildGoModule {
      pname = "hanare";
      version = "0.2.0";
      src = ./cmd/hanare;
      vendorHash = "sha256-OITq/wJbuNZzgwsqFYqKmeK8cC7WliMAFZEpkbpdO7g=";
      env.CGO_ENABLED = 0;
      ldflags = [ "-s" "-w" ];
    };
  };
}
