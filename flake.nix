{
  description = "antic: run CLI tools in isolated NixOS guests";

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

      # antic (host CLI), antic-agent (used by the module) and the static
      # cloud-hypervisor "antic update" installs.
      packages = forAll (pkgs:
        let p = import ./nix/packages.nix { inherit pkgs; };
        in p // { default = p.antic; });

      checks = forAll (pkgs: import ./nix/checks.nix { inherit pkgs; });
    };
}
