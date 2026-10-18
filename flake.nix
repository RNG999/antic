{
  description = "hanare: NixOS VM sandbox for CLI tools";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { nixpkgs, ... }:
    let
      packages = import ./nix/packages.nix { pkgs = nixpkgs.legacyPackages.x86_64-linux; };
    in
    {
      # Guest side; import from your guest flake (see examples/guest).
      nixosModules.default = ./guest/module.nix;

      # "hanare update" builds these and installs them to ~/.local/bin and
      # ~/.local/libexec/hanare.
      packages.x86_64-linux = {
        default = packages.hanare;
        inherit (packages) hanare vhost-device-vsock;
      };
    };
}
