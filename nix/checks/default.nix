{
  pkgs,
  selfPackages,
  nixosModule,
  treefmtCheck,
}:
let
  inherit (pkgs) lib;
in
{
  treefmt = treefmtCheck;

  nixos-test = import ./nixos-test.nix {
    inherit pkgs;
    module = nixosModule;
  };

  go-tests = selfPackages.seaweed-evictor.overrideAttrs (old: {
    doCheck = true;
    # The integration tests start real SeaweedFS instances and skip without weed.
    nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.seaweedfs ];
    # The fluent receiver tests listen on loopback.
    __darwinAllowLocalNetworking = true;
    checkPhase = ''
      runHook preCheck
      export GOMAXPROCS=$NIX_BUILD_CORES
      go test -race -p "$NIX_BUILD_CORES" ./...
      runHook postCheck
    '';
    installPhase = "touch $out";
  });

  golangci-lint = selfPackages.seaweed-evictor.overrideAttrs (old: {
    # The default source set has no lint config, and golangci-lint would then
    # silently fall back to its defaults.
    src = lib.fileset.toSource {
      root = ../..;
      fileset = lib.fileset.unions [
        (lib.fileset.fromSource old.src)
        ../../.golangci.yml
      ];
    };
    nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.golangci-lint ];
    buildPhase = ''
      HOME=$TMPDIR
      # golangci-lint defaults to all CPUs, which exhausts process limits on
      # shared builders.
      export GOMAXPROCS=$NIX_BUILD_CORES
      golangci-lint run --concurrency "$NIX_BUILD_CORES"
    '';
    installPhase = "touch $out";
  });
}
