{
  description = "hanare: run CLI tools in isolated NixOS guests";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      # Guest side; import from your guest flake (see examples/guest).
      nixosModules.guest = ./nix/module.nix;
      nixosModules.default = ./nix/module.nix;

      # hanare (host CLI), banken (used by the module) and the static
      # cloud-hypervisor "hanare upgrade" installs.
      packages = forAll (pkgs:
        let p = import ./nix/packages.nix { inherit pkgs; };
        in p // { default = p.hanare; });

      checks = forAll (pkgs: import ./nix/checks.nix { inherit pkgs; });
    };
}
