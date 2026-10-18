{
  description = "hanare guest configuration";

  inputs = {
    hanare.url = "github:RNG999/hanare";
    nixpkgs.follows = "hanare/nixpkgs";
  };

  outputs = { hanare, nixpkgs, ... }: {
    # "hanare guest apply" builds nixosConfigurations.<attr> (attr in hanare.toml).
    nixosConfigurations.hanare = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [ hanare.nixosModules.guest ./configuration.nix ];
    };
  };
}
