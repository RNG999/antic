{
  description = "hanare guest configuration";

  inputs = {
    hanare.url = "github:RNG999/hanare";
    nixpkgs.follows = "hanare/nixpkgs";
  };

  outputs = { hanare, nixpkgs, ... }: {
    # "hanare guest apply" and "hanare guest reset" build nixosConfigurations.<attr>
    # (attr in hanare.toml, default "hanare").
    nixosConfigurations.hanare = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [ hanare.nixosModules.default ./configuration.nix ];
    };
  };
}
