{ pkgs, selfPackages }:
{
  default = pkgs.mkShell {
    GOROOT = "${pkgs.go}/share/go";

    packages = [
      pkgs.bashInteractive
      pkgs.delve
      pkgs.gotools
      pkgs.golangci-lint
      pkgs.gopls
      pkgs.seaweedfs
    ];

    inputsFrom = [ selfPackages.seaweed-evictor ];

    shellHook = ''
      # this is only needed for hermetic builds
      unset GO_NO_VENDOR_CHECKS GOSUMDB GOPROXY GOFLAGS
    '';
  };
}
