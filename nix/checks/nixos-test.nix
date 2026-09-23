# End to end: rustfs is the upstream bucket, weed is the cache in front of it,
# and the NixOS module runs the evictor. Containers are enough because nothing
# here needs its own kernel.
{ pkgs, module }:
let
  accessKey = "BKIKJAA5BMMU2RHO6IBB";
  secretKey = "V7f1CwQqAcwo80UEIJEjc5gVQUSSx5ohQ9GSrr12";
  region = "us-east-1";

  auditConfig = pkgs.writeText "audit.json" (
    builtins.toJSON {
      fluent_host = "127.0.0.1";
      fluent_port = 24224;
    }
  );

  # Reads the shell script on stdin; weed shell exits on `exit`.
  mountScript = pkgs.writeText "mount.weed" ''
    remote.configure -name=up -type=s3 -s3.access_key=${accessKey} -s3.secret_key=${secretKey} -s3.region=${region} -s3.endpoint=http://upstream:9000 -s3.force_path_style=true
    remote.mount -dir=/buckets/cache -remote=up/origin
    exit
  '';
in
pkgs.testers.runNixOSTest {
  name = "seaweed-evictor";

  containers.upstream = {
    services.rustfs = {
      enable = true;
      environmentFile = toString (
        pkgs.writeText "rustfs.env" ''
          RUSTFS_ACCESS_KEY=${accessKey}
          RUSTFS_SECRET_KEY=${secretKey}
        ''
      );
    };
    networking.firewall.allowedTCPPorts = [ 9000 ];
  };

  containers.cache =
    { pkgs, ... }:
    {
      imports = [ module ];

      environment.systemPackages = [
        pkgs.minio-client
        pkgs.seaweedfs
      ];

      environment.variables = {
        AWS_ACCESS_KEY_ID = accessKey;
        AWS_SECRET_ACCESS_KEY = secretKey;
      };

      systemd.services.weed = {
        wantedBy = [ "multi-user.target" ];
        environment = {
          AWS_ACCESS_KEY_ID = accessKey;
          AWS_SECRET_ACCESS_KEY = secretKey;
        };
        serviceConfig = {
          ExecStart = builtins.concatStringsSep " " [
            "${pkgs.seaweedfs}/bin/weed mini"
            "-dir=/var/lib/weed -ip=127.0.0.1"
            "-master.telemetry=false -admin.ui=false -s3.port.iceberg=0 -s3.port.lance=0"
            "-filer.allowUntrustedRemoteEndpoints -s3.allowUntrustedRemoteEndpoints"
            "-s3.auditLogConfig=${auditConfig}"
          ];
          StateDirectory = "weed";
        };
      };

      # Started by the test once the bucket is mounted.
      systemd.services.weed-remote-sync = {
        serviceConfig.ExecStart = "${pkgs.seaweedfs}/bin/weed filer.remote.sync -filer=127.0.0.1:8888 -dir=/buckets/cache";
        serviceConfig.Restart = "on-failure";
      };

      services.seaweed-evictor = {
        enable = true;
        capacity = "3MiB";
        filer = "127.0.0.1:18888";
        logLevel = "debug";
      };
    };

  testScript = ''
    def sha(cmd):
        return cache.succeed(cmd + " | sha256sum").split()[0]

    start_all()

    upstream.wait_for_unit("rustfs.service")
    upstream.wait_for_open_port(9000)
    cache.wait_for_unit("weed.service")
    cache.wait_for_open_port(8333)
    cache.wait_for_open_port(18888)

    cache.succeed("mc alias set up http://upstream:9000 ${accessKey} ${secretKey}")
    cache.succeed("mc alias set local http://127.0.0.1:8333 ${accessKey} ${secretKey}")
    cache.succeed("mc mb up/origin")

    cache.succeed("weed shell -master=127.0.0.1:9333 < ${mountScript}")
    cache.succeed("systemctl start weed-remote-sync.service")
    cache.wait_for_unit("seaweed-evictor.service")
    cache.wait_until_succeeds("journalctl -u seaweed-evictor | grep 'initial scan done'", timeout=60)

    # Six 1 MiB objects into a 3 MiB cache.
    for i in range(6):
        cache.succeed(f"head -c 1048576 /dev/urandom > /tmp/obj-{i}")
        cache.succeed(f"mc cp /tmp/obj-{i} local/cache/obj-{i}")

    with subtest("objects are uploaded to the upstream in the background"):
        cache.wait_until_succeeds("test $(mc ls up/origin | wc -l) -eq 6", timeout=90)

    with subtest("the evictor drops local copies down to the capacity"):
        cache.wait_until_succeeds("test $(journalctl -u seaweed-evictor | grep -c 'msg=uncached') -ge 3", timeout=60)

    with subtest("evicted objects still read back, from the upstream"):
        for i in range(6):
            expected = sha(f"cat /tmp/obj-{i}")
            assert sha(f"mc cat local/cache/obj-{i}") == expected, f"obj-{i} differs through the cache"
            assert sha(f"mc cat up/origin/obj-{i}") == expected, f"obj-{i} differs upstream"

    with subtest("the gateway sends its read log to the evictor"):
        # weed connects to the receiver on its first audit event.
        cache.wait_until_succeeds("ss -tn state established '( sport = :24224 )' | grep -q 24224", timeout=30)
  '';
}
