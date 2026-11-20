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
  uuid2 = "f47ac10b-58cc-4372-a567-0e02b2c3d479";
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
          peers = [
            { uuid = uuid0; allowedIPs = [ "10.7.0.2/32" ]; }
            # 198.51.100.0/24 (TEST-NET-2, standing in for the MS CIDRs) is
            # routed into the tunnel toward c2, where its proxyRules
            # intercept the range in-process.
            { uuid = uuid2; allowedIPs = [ "10.7.0.3/32" "198.51.100.0/24" ]; }
          ];
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

      # --- mock corporate HTTP proxy (:8888) + TLS backend (:1443) ---------
      # The proxy allows CONNECT to ms-test.example.com only (403 for IP
      # literals and other hosts) and records every CONNECT target to
      # /tmp/proxy-connects.log. The TLS backend serves HTTPS 200 "ok".
      systemd.services.mock-proxy = {
        description = "mock corporate HTTP proxy";
        wantedBy = [ "multi-user.target" ];
        serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 /etc/mock-proxy.py";
      };
      environment.etc."mock-proxy.py".text = ''
        import socket, threading

        ALLOWED_HOST = "ms-test.example.com"

        def log(target):
            with open("/tmp/proxy-connects.log", "a") as f:
                f.write(target + "\n")

        def handle(conn):
            f = conn.makefile("rb")
            target = None
            while True:
                line = f.readline()
                if not line:
                    conn.close(); return
                line = line.decode(errors="replace").strip()
                if line == "":
                    break
                parts = line.split()
                if len(parts) == 3 and parts[0] == "CONNECT":
                    target = parts[1]
            if target is None:
                conn.close(); return
            log(target)
            if target.rsplit(":", 1)[0] == ALLOWED_HOST:
                conn.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
                try:
                    up = socket.create_connection(("127.0.0.1", 1443), timeout=5)
                    def pump(a, b):
                        try:
                            while True:
                                d = a.recv(4096)
                                if not d: break
                                b.sendall(d)
                        except OSError:
                            pass
                        finally:
                            for x in (a, b):
                                try: x.close()
                                except OSError: pass
                    threading.Thread(target=pump, args=(conn, up), daemon=True).start()
                    pump(up, conn)
                except OSError:
                    conn.close()
            else:
                conn.sendall(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
                conn.close()

        srv = socket.socket()
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        srv.bind(("127.0.0.1", 8888))
        srv.listen(64)
        while True:
            c, _ = srv.accept()
            threading.Thread(target=handle, args=(c,), daemon=True).start()
      '';
      systemd.services.mock-tls = {
        description = "mock TLS backend for ms-test.example.com";
        wantedBy = [ "multi-user.target" ];
        preStart = ''
          ${pkgs.openssl}/bin/openssl req -x509 -newkey rsa:2048 -nodes \
            -keyout /run/mock-tls-key.pem -out /run/mock-tls-cert.pem -days 2 \
            -subj "/CN=ms-test.example.com" \
            -addext "subjectAltName=DNS:ms-test.example.com" >/dev/null 2>&1
          cat /run/mock-tls-key.pem /run/mock-tls-cert.pem > /run/mock-tls-combined.pem
        '';
        serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 /etc/mock-tls.py";
      };
      environment.etc."mock-tls.py".text = ''
        import http.server, ssl
        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", "2")
                self.end_headers()
                self.wfile.write(b"ok")
            def log_message(self, *a):
                pass
        httpd = http.server.HTTPServer(("127.0.0.1", 1443), H)
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain("/run/mock-tls-combined.pem")
        httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
        httpd.serve_forever()
      '';

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
        c2 = {
          # Split-dataplane: 198.51.100.0/24 (TEST-NET-2, standing in for
          # the MS CIDRs) is intercepted in-process and carried through the
          # local mock proxy with SNI-based CONNECT; the kernel path (ping,
          # plain TCP to 10.7.0.x) stays untouched.
          address = [ "10.7.0.3/24" ];
          privateKeyFile = "/run/secrets/ripws-c2.key";
          preStart = "install -d -m 700 /run/secrets && echo '${psk}' > /run/secrets/ripws-c2.key";
          peers = [ {
            uuid = uuid2;
            endpoint = "ws://server:8080";
            allowedIPs = [ "10.7.0.0/24" ];
          } ];
          proxyRules = [ {
            cidrs = [ "198.51.100.0/24" ];
            proxy = "http://127.0.0.1:8888";
            sni = true;
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
    client.wait_for_unit("ripws-c2.service")
    client.wait_for_unit("mock-proxy.service")
    client.wait_for_unit("mock-tls.service")

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

    # --- split-dataplane proxy + SNI E2E ---------------------------------------
    # The 198.51.100.0/24 range exists nowhere on the VM network and the
    # client has no route/ip_forward/NAT for it: if the packet reached the
    # kernel, this test could never pass. Only the in-process gVisor proxy
    # dataplane with SNI-based CONNECT can answer.
    with subtest("split proxy SNI"):
        client.succeed("rm -f /tmp/proxy-connects.log")
        # kernel dataplane must still work alongside the proxy path
        # (10.7.0.3 is c2's TUN address — served by the kernel, not the proxy)
        server.succeed("ping -c 2 -W 2 10.7.0.3")
        # HTTPS to the "MS" range: SNI extracted, CONNECT ms-test.example.com
        out = server.succeed(
            "curl -s --max-time 20 --resolve ms-test.example.com:443:198.51.100.10 https://ms-test.example.com/"
        )
        assert "ok" in out, out
        connects = client.succeed("cat /tmp/proxy-connects.log")
        assert "ms-test.example.com:443" in connects, connects
        # the kernel path (ping/SSH) never shows up in the proxy log
        client.succeed("! grep -q '10\\.7\\.0' /tmp/proxy-connects.log")
        client.succeed("! grep -q '10\\.9\\.0' /tmp/proxy-connects.log")

    with subtest("split proxy UDP refused"):
        # UDP to the proxied range must fail fast, not hang or leak.
        server.fail("timeout 10 bash -c 'echo x | nc -u -w 5 198.51.100.10 443' >/dev/null 2>&1; test $? -ne 124")

    with subtest("split proxy survives restart"):
        client.execute("systemctl restart ripws-c2.service")
        client.wait_for_unit("ripws-c2.service")
        time.sleep(3)
        client.succeed("rm -f /tmp/proxy-connects.log")
        out = server.succeed(
            "curl -s --max-time 30 --resolve ms-test.example.com:443:198.51.100.10 https://ms-test.example.com/"
        )
        assert "ok" in out, out

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
