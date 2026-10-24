{
  description = "antic guest configuration";

  inputs = {
    antic.url = "github:RNG999/antic";
    nixpkgs.follows = "antic/nixpkgs";
  };

  outputs = { antic, nixpkgs, ... }: {
    # "antic guest apply" builds nixosConfigurations.<attr> (attr in antic.toml).
    nixosConfigurations.antic = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [ antic.nixosModules.guest ./configuration.nix ];
    };
  };
}
