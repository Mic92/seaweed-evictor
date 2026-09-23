{ pkgs }:
rec {
  seaweed-evictor = pkgs.callPackage ./seaweed-evictor.nix { };
  dev = pkgs.callPackage ../dev { inherit seaweed-evictor; };
  default = seaweed-evictor;
}
