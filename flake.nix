{
  description = "Bound a SeaweedFS remote-mounted S3 cache with S3-FIFO eviction";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable-small";
  };

  outputs =
    inputs@{ nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];

      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system nixpkgs.legacyPackages.${system});

      treefmt = pkgs: import ./nix/formatter { inherit pkgs; };
    in
    {
      nixosModules = {
        seaweed-evictor = import ./nix/nixosModules/seaweed-evictor.nix inputs.self;
        default = inputs.self.nixosModules.seaweed-evictor;
      };

      packages = forAllSystems (_: pkgs: import ./nix/packages { inherit pkgs; });

      checks = forAllSystems (
        system: pkgs:
        import ./nix/checks {
          inherit pkgs;
          selfPackages = inputs.self.packages.${system};
          nixosModule = inputs.self.nixosModules.default;
          treefmtCheck = (treefmt pkgs).check inputs.self;
        }
      );

      devShells = forAllSystems (
        system: pkgs:
        import ./nix/devshells {
          inherit pkgs;
          selfPackages = inputs.self.packages.${system};
        }
      );

      formatter = forAllSystems (_: pkgs: (treefmt pkgs).wrapper);
    };
}
