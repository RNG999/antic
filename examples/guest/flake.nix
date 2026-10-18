{
  description = "hanare guest configuration";

  inputs = {
    hanare.url = "github:RNG999/hanare";
    nixpkgs.follows = "hanare/nixpkgs";
  };

  outputs = { hanare, nixpkgs, ... }: {
    # "hanare switch" and "hanare reinstall" build nixosConfigurations.hanare.
    nixosConfigurations.hanare = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [ hanare.nixosModules.default ./configuration.nix ];
    };
  };
}
