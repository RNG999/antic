{
  description = "hanare: NixOS VM sandbox for CLI tools";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs, ... }: {
    nixosConfigurations.hanare = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [ ./configuration.nix ];
    };
    packages.x86_64-linux = {
      # VM disk image; "hanare reinstall" builds and installs it.
      image = self.nixosConfigurations.hanare.config.system.build.images.raw-efi;

      # Host-side CLI (static); "hanare update" builds and installs it to ~/.local/bin.
      hanare = nixpkgs.legacyPackages.x86_64-linux.buildGoModule {
        pname = "hanare";
        version = "0.1.0";
        src = ./cmd/hanare;
        vendorHash = "sha256-dVtheik9hgc8Ig52xOR7v1ujtqj2bzV1+gl5KmKBB98=";
        env.CGO_ENABLED = 0;
        ldflags = [ "-s" "-w" ];
      };
    };
  };
}
