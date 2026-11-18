/*
   End-to-end NixOS VM test for the services.ripws module.

   Two VMs (server, client) each running two tunnels:
     s0/c0  kernel ↔ kernel   (10.7.0.x, inline privateKey)
     s1/c1  userspace server ↔ kernel client (10.9.0.x, privateKeyFile —
            exercises the /run secret substitution; gVisor netstack
            answers ICMP on the server side)

   Checks: config file perms & content, interface state, bidirectional
   ping, restart → reconnect, stop → interface cleanup, proxyEnvironment
   rendering.

   Run:  nix build .#checks.x86_64-linux.ripws-vm -L
   (or:  nix-build -E 'import ./nix/vm-test.nix { ripwsPackage = (import ./nix/package.nix) { }; }' --argstr ... —
   the flake check is the supported entry point.)
*/
{ pkgs ? import <nixpkgs> { }, ripwsPackage ? pkgs.ripws }:

let
  psk = "SCCEuJtqD1CFE7y9f332WQ6H9o7Lr5TgKQZ4E4Q8Fp0=";
  uuid0 = "550e8400-e29b-41d4-a716-446655440000";
  uuid1 = "6ba7b810-9dad-41d4-80b4-00c04fd430c8";
in
pkgs.testers.runNixOSTest {
  name = "ripws";

  nodes = {
    server = { ... }: {
      imports = [ ./module.nix ];
      networking.firewall.enable = false;
      services.ripws.package = ripwsPackage;
      services.ripws.tunnels = {
        s0 = {
          address = [ "10.7.0.1/24" ];
          listenPort = 8080;
          privateKey = psk;
          peers = [ { uuid = uuid0; allowedIPs = [ "10.7.0.2/32" ]; } ];
        };
        s1 = {
          mode = "userspace";
          address = [ "10.9.0.1/24" ];
          listenPort = 8081;
          privateKeyFile = "/run/secrets/ripws-s1.key";
          # test fixture: provision the "secret" at service start
          preStart = "install -d -m 700 /run/secrets && echo '${psk}' > /run/secrets/ripws-s1.key";
          peers = [ { uuid = uuid1; allowedIPs = [ "10.9.0.2/32" ]; } ];
        };
      };
    };

    client = { ... }: {
      imports = [ ./module.nix ];
      networking.firewall.enable = false;
      services.ripws.package = ripwsPackage;
      services.ripws.tunnels = {
        c0 = {
          address = [ "10.7.0.2/24" ];
          privateKeyFile = "/run/secrets/ripws-c0.key";
          preStart = "install -d -m 700 /run/secrets && echo '${psk}' > /run/secrets/ripws-c0.key";
          proxyEnvironment = { NO_PROXY = "localhost, 127.0.0.1"; };
          peers = [ {
            uuid = uuid0;
            endpoint = "ws://server:8080";
            allowedIPs = [ "10.7.0.1/32" ];
          } ];
        };
        c1 = {
          address = [ "10.9.0.2/24" ];
          privateKeyFile = "/run/secrets/ripws-c1.key";
          preStart = "install -d -m 700 /run/secrets && echo '${psk}' > /run/secrets/ripws-c1.key";
          peers = [ {
            uuid = uuid1;
            endpoint = "ws://server:8081";
            allowedIPs = [ "10.9.0.1/32" ];
          } ];
        };
      };
    };
  };

  testScript = ''
    import time

    server.start()
    client.start()
    server.wait_for_unit("multi-user.target")
    client.wait_for_unit("multi-user.target")

    server.wait_for_open_port(8080)
    server.wait_for_open_port(8081)
    server.wait_for_unit("ripws-s0.service")
    server.wait_for_unit("ripws-s1.service")
    client.wait_for_unit("ripws-c0.service")
    client.wait_for_unit("ripws-c1.service")

    # --- runtime config files -------------------------------------------------
    with subtest("runtime config rendering"):
        client.succeed("stat -c %a /run/ripws | grep -qx 700")
        for ifn in ("c0", "c1"):
            client.succeed(f"stat -c %a /run/ripws/{ifn}.conf | grep -qx 600")
            # secrets substituted, no placeholders left
            client.succeed(f"grep -qx 'PrivateKey = ${psk}' /run/ripws/{ifn}.conf")
            client.fail(f"grep -q '@@' /run/ripws/{ifn}.conf")
            client.succeed(f"grep -q 'Mode = kernel' /run/ripws/{ifn}.conf")
        server.succeed("grep -q 'Mode = userspace' /run/ripws/s1.conf")
        server.succeed("grep -qx 'PrivateKey = ${psk}' /run/ripws/s1.conf")
        server.succeed("grep -q 'ListenPort = 8081' /run/ripws/s1.conf")

    # --- proxy environment rendering -----------------------------------------
    with subtest("proxyEnvironment"):
        out = client.succeed("systemctl show ripws-c0.service -p Environment")
        assert "NO_PROXY=localhost, 127.0.0.1" in out, out

    # --- kernel tunnel up ------------------------------------------------------
    with subtest("kernel tunnel connectivity"):
        client.wait_until_succeeds("ip addr show c0 | grep -q '10.7.0.2'", timeout=30)
        server.wait_until_succeeds("ip addr show s0 | grep -q '10.7.0.1'", timeout=30)
        # give the WebSocket session a moment after interface creation
        time.sleep(3)
        client.succeed("ping -c 3 -W 2 10.7.0.1")
        server.succeed("ping -c 3 -W 2 10.7.0.2")
        # route installed per AllowedIPs
        client.succeed("ip route show 10.7.0.1/32 | grep -q dev")

    # --- userspace server answers ICMP via gVisor ------------------------------
    with subtest("userspace tunnel connectivity"):
        client.wait_until_succeeds("ip addr show c1 | grep -q '10.9.0.2'", timeout=30)
        time.sleep(3)
        client.succeed("ping -c 3 -W 2 10.9.0.1")

    # --- restart: clean teardown + reconnect -----------------------------------
    with subtest("restart reconnects"):
        client.execute("systemctl restart ripws-c0.service")
        client.wait_for_unit("ripws-c0.service")
        time.sleep(3)
        client.wait_until_succeeds("ping -c 1 -W 2 10.7.0.1", timeout=60)
        # no leaked duplicate interfaces
        out = client.succeed("ip -o link show | grep -c 'c0:'")
        assert out.strip() == "1", f"expected exactly one c0 interface, got: {out}"

    # --- stop: interface removed ------------------------------------------------
    with subtest("stop cleans up"):
        client.execute("systemctl stop ripws-c0.service")
        client.wait_until_succeeds("! ip link show c0", timeout=30)

    # --- flapping server: client must come back ---------------------------------
    with subtest("server restart recovers"):
        client.execute("systemctl start ripws-c0.service")
        client.wait_for_unit("ripws-c0.service")
        server.execute("systemctl restart ripws-s0.service")
        server.wait_for_unit("ripws-s0.service")
        client.wait_until_succeeds("ping -c 1 -W 2 10.7.0.1", timeout=90)
  '';
}
