self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.seaweed-evictor;
in
{
  options.services.seaweed-evictor = {
    enable = lib.mkEnableOption "seaweed-evictor, S3-FIFO eviction for a SeaweedFS remote-mounted bucket";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.seaweed-evictor;
      defaultText = lib.literalExpression "seaweed-evictor.packages.\${system}.seaweed-evictor";
      description = "The seaweed-evictor package.";
    };

    capacity = lib.mkOption {
      type = lib.types.str;
      example = "500GiB";
      description = "Size limit of the local cache, as a byte count or a size like `10GiB`.";
    };

    filer = lib.mkOption {
      type = lib.types.str;
      default = "localhost:18888";
      description = "Address of the filer's gRPC port, which is its HTTP port plus 10000.";
    };

    root = lib.mkOption {
      type = lib.types.str;
      default = "/buckets";
      description = "Filer directory to manage.";
    };

    fluentListen = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = "127.0.0.1:24224";
      description = ''
        Address that receives the S3 gateway's audit log, which is how reads are
        seen. Point `weed s3 -auditLogConfig` at it. `null` disables read
        tracking and eviction becomes plain FIFO.
      '';
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [
        "debug"
        "info"
        "warn"
        "error"
      ];
      default = "info";
      description = "Log level.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.seaweed-evictor = {
      description = "seaweed-evictor";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];

      environment = {
        SEAWEED_EVICTOR_CAPACITY = cfg.capacity;
        SEAWEED_EVICTOR_FILER = cfg.filer;
        SEAWEED_EVICTOR_ROOT = cfg.root;
        SEAWEED_EVICTOR_LOG_LEVEL = cfg.logLevel;
      }
      // lib.optionalAttrs (cfg.fluentListen != null) {
        SEAWEED_EVICTOR_FLUENT_LISTEN = cfg.fluentListen;
      };

      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        DynamicUser = true;
        # The filer may start after us; the evictor exits when it cannot reach it.
        Restart = "always";
        RestartSec = "5s";

        # Hardening
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectControlGroups = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        RemoveIPC = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
        SystemCallArchitectures = "native";
      };
    };
  };
}
