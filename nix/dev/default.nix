# `nix run .#dev` starts a local write-back cache for live testing. Point the
# S3 client at the "cache" gateway:
#
#   client -> weed "cache" (S3 gateway, filer) -> weed "upstream" (stands in for real S3)
#                 ^ seaweed-evictor bounds its size, filer.remote.sync uploads lazily
{
  lib,
  pkgs,
  seaweed-evictor,
}:
let
  yaml = pkgs.formats.yaml { };

  weed = lib.getExe pkgs.seaweedfs;

  accessKey = "test";
  secretKey = "testtest12345"; # weed requires at least 8 characters
  region = "us-east-1";
  bucket = "cache";

  # Every weed port must have its gRPC twin (port + 10000) free.
  ports = {
    upstream = {
      master = 19333;
      volume = 18080;
      filer = 18888;
      s3 = 18333;
      admin = 23646;
      webdav = 17333;
    };
    cache = {
      master = 39333;
      volume = 38080;
      filer = 38888;
      s3 = 38333;
      admin = 43646;
      webdav = 37333;
    };
    # HTTPS twin of cache.s3, only served with SEAWEED_EVICTOR_TLS=1.
    s3https = 38443;
    fluent = 24224;
  };

  # $DATA is expanded by the shell that process-compose starts, so the
  # directory argument is double-quoted instead of going through escapeShellArgs.
  mini =
    name: extra:
    let
      p = ports.${name};
    in
    lib.concatStringsSep " " (
      [
        "mini"
        "\"-dir=$DATA/${name}\""
        "-ip=127.0.0.1"
        "-ip.bind=127.0.0.1"
        "-master.port=${toString p.master}"
        "-volume.port=${toString p.volume}"
        "-filer.port=${toString p.filer}"
        "-s3.port=${toString p.s3}"
        "-admin.port=${toString p.admin}"
        "-webdav.port=${toString p.webdav}"
        "-s3.port.iceberg=0"
        "-s3.port.lance=0"
        "-master.telemetry=false"
        "-admin.ui=false"
      ]
      ++ map lib.escapeShellArg extra
    );

  auditConfig = pkgs.writeText "audit.json" (
    builtins.toJSON {
      fluent_host = "127.0.0.1";
      fluent_port = ports.fluent;
    }
  );

  # Unsigned requests are the "anonymous" identity. Reads only, so nix can
  # fetch from the cache without listing or writing to it.
  anonymousConfig = pkgs.writeText "s3-anonymous.json" (
    builtins.toJSON {
      identities = [
        {
          name = "admin";
          credentials = [
            {
              inherit accessKey secretKey;
            }
          ];
          actions = [
            "Admin"
            "Read"
            "Write"
            "List"
            "Tagging"
          ];
        }
        {
          name = "anonymous";
          actions = [ "Read:${bucket}" ];
        }
      ];
    }
  );

  http = port: path: {
    http_get = {
      host = "127.0.0.1";
      inherit port path;
    };
    period_seconds = 2;
    timeout_seconds = 2;
  };

  config = yaml.generate "process-compose.yaml" {
    version = "0.5";
    environment = [
      "AWS_ACCESS_KEY_ID=${accessKey}"
      "AWS_SECRET_ACCESS_KEY=${secretKey}"
    ];
    processes = {
      upstream = {
        command = "${weed} ${mini "upstream" [ "-bucket=upstream" ]}";
        readiness_probe = http ports.upstream.s3 "/status";
      };

      cache = {
        command = "${weed} ${
          mini "cache" [
            "-filer.allowUntrustedRemoteEndpoints"
            "-s3.allowUntrustedRemoteEndpoints"
            "-s3.auditLogConfig=${auditConfig}"
          ]
        } $S3_EXTRA_ARGS";
        readiness_probe = http ports.cache.filer "/";
      };

      mount = {
        # Idempotent: remounting an already mounted directory fails harmlessly.
        command = ''
          printf '%s\n' \
            'remote.configure -name=up -type=s3 -s3.access_key=${accessKey} -s3.secret_key=${secretKey} -s3.region=${region} -s3.endpoint=http://127.0.0.1:${toString ports.upstream.s3} -s3.force_path_style=true' \
            'remote.mount -dir=/buckets/${bucket} -remote=up/upstream' \
            'exit' |
            ${weed} shell -master=127.0.0.1:${toString ports.cache.master} || true
        '';
        depends_on = {
          upstream.condition = "process_healthy";
          cache.condition = "process_healthy";
        };
        availability.restart = "no";
      };

      remote-sync = {
        command = "${weed} filer.remote.sync -filer=127.0.0.1:${toString ports.cache.filer} -dir=/buckets/${bucket}";
        depends_on.mount.condition = "process_completed";
        availability.restart = "on_failure";
      };

      evictor = {
        command = "${seaweed-evictor}/bin/seaweed-evictor";
        # The evictor is configured through its environment; a capacity set in
        # the caller's environment takes precedence over this default.
        environment = [
          "SEAWEED_EVICTOR_FILER=127.0.0.1:${toString (ports.cache.filer + 10000)}"
          "SEAWEED_EVICTOR_ROOT=/buckets"
          "SEAWEED_EVICTOR_CAPACITY=\${SEAWEED_EVICTOR_CAPACITY:-1GiB}"
          "SEAWEED_EVICTOR_FLUENT_LISTEN=127.0.0.1:${toString ports.fluent}"
          "SEAWEED_EVICTOR_LOG_LEVEL=debug"
        ];
        depends_on.mount.condition = "process_completed";
        availability.restart = "on_failure";
      };
    };
  };
in
pkgs.writeShellApplication {
  name = "seaweed-evictor-dev";
  runtimeInputs = [
    pkgs.process-compose
    pkgs.openssl
  ];
  text = ''
    DATA="''${SEAWEED_EVICTOR_DATA:-$PWD/.data}"
    export DATA
    mkdir -p "$DATA"

    S3_EXTRA_ARGS=""

    # SEAWEED_EVICTOR_ANONYMOUS_READ=1 lets unsigned GET/HEAD read the bucket.
    if [ "''${SEAWEED_EVICTOR_ANONYMOUS_READ:-0}" = 1 ]; then
      S3_EXTRA_ARGS="$S3_EXTRA_ARGS -s3.config=${anonymousConfig}"
    fi

    # SEAWEED_EVICTOR_TLS=1 also serves HTTPS with a self-signed certificate. Clients
    # trust it via SSL_CERT_FILE=$DATA/tls/cert.pem.
    if [ "''${SEAWEED_EVICTOR_TLS:-0}" = 1 ]; then
      if [ ! -f "$DATA/tls/cert.pem" ]; then
        mkdir -p "$DATA/tls"
        openssl req -x509 -newkey rsa:2048 -nodes -days 365 -subj /CN=localhost \
          -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" \
          -keyout "$DATA/tls/key.pem" -out "$DATA/tls/cert.pem" 2>/dev/null
      fi
      S3_EXTRA_ARGS="$S3_EXTRA_ARGS -s3.port.https=${toString ports.s3https} -s3.cert.file=$DATA/tls/cert.pem -s3.key.file=$DATA/tls/key.pem"
    fi
    export S3_EXTRA_ARGS

    cat >&2 <<EOF
    S3 endpoint:  127.0.0.1:${toString ports.cache.s3} (plain HTTP, path-style)
    bucket:       ${bucket}
    region:       ${region}
    access key:   ${accessKey}
    secret key:   ${secretKey}
    upstream S3:  127.0.0.1:${toString ports.upstream.s3} (bucket "upstream")
    EOF

    exec process-compose up -f ${config} -p "''${SEAWEED_EVICTOR_UI_PORT:-18081}" "$@"
  '';
}
