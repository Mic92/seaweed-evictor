{ buildGoModule, lib }:

buildGoModule {
  pname = "seaweed-evictor";
  version = "0.1.0";
  vendorHash = "sha256-OK4ptCD36/OIUIXVg50V6eu8k01DGoqg07Apa8H5nZo=";

  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ../../go.mod
      ../../go.sum
      ../../cmd
      ../../internal
      ../../s3fifo
    ];
  };

  subPackages = [ "cmd/seaweed-evictor" ];

  # The Go tests run in checks.go-tests so they can be cached separately.
  doCheck = false;

  meta.mainProgram = "seaweed-evictor";
}
