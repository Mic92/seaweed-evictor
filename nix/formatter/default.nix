{ pkgs }:
let
  treefmt = pkgs.treefmt.withConfig {
    runtimeInputs = [
      pkgs.nixfmt
      pkgs.deadnix
      pkgs.gofumpt
      pkgs.yamlfmt
      pkgs.mdformat
    ];

    settings = {
      excludes = [ ".envrc" ];

      formatter = {
        nixfmt = {
          command = "nixfmt";
          includes = [ "*.nix" ];
        };
        deadnix = {
          command = "deadnix";
          options = [ "--edit" ];
          includes = [ "*.nix" ];
          # Runs before nixfmt so the result is formatted.
          priority = -1;
        };
        gofumpt = {
          command = "gofumpt";
          options = [ "-w" ];
          includes = [ "*.go" ];
        };
        yamlfmt = {
          command = "yamlfmt";
          includes = [
            "*.yaml"
            "*.yml"
          ];
        };
        mdformat = {
          command = "mdformat";
          includes = [ "*.md" ];
        };
      };
    };
  };
in
{
  wrapper = treefmt;
  inherit (treefmt) check;
}
