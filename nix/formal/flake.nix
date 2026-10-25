{
  description = "antic formal verification guest environment";

  inputs = {
    antic.url = "github:RNG999/antic";
    nixpkgs.follows = "antic/nixpkgs";
  };

  outputs = { self, antic, nixpkgs, ... }: {
    nixosConfigurations.formal = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        antic.nixosModules.guest
        ({ pkgs, ... }: {
          antic.user = "thetaru";
          environment.systemPackages = with pkgs; [
            git
            z3
            (python3.withPackages (ps: with ps; [
              z3-solver
            ]))
          ];
        })
      ];
    };
  };
}
