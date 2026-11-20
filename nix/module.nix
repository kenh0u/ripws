/*
   NixOS module for ripws — Raw IP over WebSocket.

   Declarative, wg-quick-style deployment:

     services.ripws.tunnels.<name> = {
       address        = [ "10.7.0.1/24" ];
       listenPort     = 8080;                   # null → client
       privateKeyFile = "/run/secrets/ripws.key";
       peers          = [ { uuid = …; allowedIPs = [ … ]; } ];
     };

   One systemd service `ripws-<name>` per tunnel, running
   `ripws up <runtime-conf>` in the foreground (Type=simple, SIGTERM-clean
   shutdown handled by ripws itself, Restart=always). Kernel mode gets
   /dev/net/tun (service runs as root, CAP_NET_ADMIN implied); userspace
   mode works unprivileged but is still started by the system manager.

   Secrets: `*Key` (inline) lands in the world-readable Nix store —
   convenience/testing only. `*KeyFile` contents never touch the store: a
   placeholder token is written at build time and substituted at service
   start into /run/ripws/<name>.conf (0600, root only), mirroring
   wireguard's two-tier style.
*/
{ config, lib, pkgs, ... }:

let
  inherit (lib)
    mkOption mkPackageOption mkIf
    types literalExpression optionalString optional optionalAttrs concatStringsSep
    imap1 nameValuePair mapAttrs' ;

  cfg = config.services.ripws;

  # SPEC 2.2: base64 of 32 random bytes → 44 chars ending in '='.
  pskType = types.strMatching "[A-Za-z0-9+/]{43}=";
  uuidType = types.strMatching
    "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}";

  peerOpts = { ... }: {
    options = {
      uuid = mkOption {
        type = uuidType;
        description = ''
          UUIDv4 peer identity: the accepted endpoint path element on the
          server, appended to the server URL on the client.
          Generate with `ripws genuuid`.
        '';
      };

      presharedKey = mkOption {
        type = types.nullOr pskType;
        default = null;
        description = ''
          Per-peer PSK overriding the interface key.

          Warning: stored in the world-readable Nix store. Prefer
          {option}`presharedKeyFile` outside of tests.
        '';
      };

      presharedKeyFile = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "/run/secrets/ripws_peer.psk";
        description = "Path to a file holding the per-peer PSK.";
      };

      allowedIPs = mkOption {
        type = with types; listOf str;
        default = [ ];
        example = [ "10.7.0.2/32" ];
        description = "CIDR routes via this peer.";
      };

      endpoint = mkOption {
        type = with types; nullOr str;
        default = null;
        example = "wss://vpn.example.net:8443";
        description = "ws(s):// server URL (client tunnels only).";
      };

      persistentKeepalive = mkOption {
        type = types.ints.between 30 300;
        default = 30;
        description = "WebSocket Ping interval in seconds.";
      };
    };
  };

  tunnelOpts = { name, config, ... }: {
    options = {
      package = mkOption {
        type = types.package;
        default = cfg.package;
        defaultText = literalExpression "config.services.ripws.package";
        description = "ripws package used for this tunnel.";
      };

      mode = mkOption {
        type = types.enum [ "kernel" "userspace" ];
        default = "kernel";
        description = ''
          `kernel`: real TUN interface configured via `ip` (privileged).
          `userspace`: gVisor netstack — no TUN, no privileges, NAT gateway.
        '';
      };

      address = mkOption {
        type = with types; listOf str;
        example = [ "10.7.0.1/24" ];
        description = "Interface addresses (CIDR).";
      };

      listenPort = mkOption {
        type = with types; nullOr types.port;
        default = null;
        example = 8080;
        description = "Server listen port. `null` makes this tunnel a client.";
      };

      privateKey = mkOption {
        type = types.nullOr pskType;
        default = null;
        description = ''
          Default PSK for peers without an explicit key.

          Warning: stored in the world-readable Nix store. Prefer
          {option}`privateKeyFile` outside of tests.
        '';
      };

      privateKeyFile = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "/run/secrets/ripws_server.key";
        description = "Path to a file holding the default PSK.";
      };

      mtu = mkOption {
        type = types.nullOr (types.ints.between 576 65535);
        default = null;
        example = 1400;
        description = "Interface MTU (ripws built-in default: 1420).";
      };

      dns = mkOption {
        type = with types; listOf str;
        default = [ ];
        example = [ "10.7.0.1" ];
        description = ''
          Nameservers injected into /etc/resolv.conf (kernel mode; ripws
          backs up and restores the original on down).
        '';
      };

      table = mkOption {
        type = types.enum [ "auto" "off" ];
        default = "auto";
        description = "auto installs AllowedIPs routes; off leaves routing to you.";
      };

      nat = mkOption {
        type = types.bool;
        default = false;
        description = "Userspace mode: enable the NAT gateway.";
      };

      peers = mkOption {
        type = types.listOf (types.submodule peerOpts);
        default = [ ];
        description = "Peer entries.";
      };

      proxyRules = mkOption {
        type = types.listOf (types.submodule {
          options = {
            cidrs = mkOption {
              type = with types; listOf str;
              example = [ "20.20.32.0/19" "40.126.0.0/18" ];
              description = "Destination CIDRs intercepted in-process and routed through the proxy.";
            };
            proxy = mkOption {
              type = types.str;
              example = "http://proxy.iiji.jp:8080";
              description = "Upstream proxy URL (http://, https://, socks5://, socks5h://; user:pass@ supported).";
            };
            sni = mkOption {
              type = types.bool;
              default = false;
              description = ''
                Peek the TLS ClientHello SNI and use it as the proxy CONNECT
                target hostname; falls back to the destination IP on non-TLS
                flows, missing SNI, or peek timeout.
              '';
            };
          };
        });
        default = [ ];
        description = ''
          Split-dataplane proxy rules: packets to these CIDRs are terminated
          in-process (gVisor netstack) and carried through the upstream proxy
          instead of the kernel TUN — Mode = kernel is kept for everything
          else (SSH, ICMP, 10.x transit all unchanged). UDP to matched CIDRs
          is refused (HTTP/SOCKS-to-TCP proxies carry no UDP).

          Note: never include the proxy server's own IP in cidrs — that would
          loop its own connections back into itself.
        '';
      };

      proxyEnvironment = mkOption {
        type = types.attrsOf types.str;
        default = { };
        example = literalExpression ''
          { ALL_PROXY = "socks5h://proxy.corp:1080"; NO_PROXY = "localhost,127.0.0.1"; }
        '';
        description = ''
          Proxy environment for the WebSocket dial (client tunnels):
          HTTP_PROXY / HTTPS_PROXY / ALL_PROXY / NO_PROXY.
        '';
      };

      preStart = mkOption {
        type = with types; lines;
        default = "";
        example = literalExpression "''${pkgs.iptables}/bin/iptables -t nat -A POSTROUTING -j MASQUERADE";
        description = "Shell commands run before the service starts (systemd ExecStartPre).";
      };

      postStart = mkOption {
        type = with types; lines;
        default = "";
        description = "Shell commands run after the service starts (systemd ExecStartPost).";
      };

      autoStart = mkOption {
        type = types.bool;
        default = true;
        description = "Pull the service into multi-user.target.";
      };

      extraPackages = mkOption {
        type = with types; listOf package;
        default = [ ];
        example = literalExpression "[ pkgs.iptables ]";
        description = "Extra packages on the service's PATH.";
      };

      runtimeConfigPath = mkOption {
        type = types.str;
        default = "/run/ripws/${name}.conf";
        internal = true;
        description = "Runtime config path (0600, root-only).";
      };
    };
  };

  # Build-time config text. File-backed secrets become placeholder tokens;
  # the start script substitutes real values right before exec.
  baseConf = t:
    let
      peerConf = idx: p: ''
        [Peer]
        UUID = ${p.uuid}
        ${optionalString (p.presharedKey != null) "PresharedKey = ${p.presharedKey}"}
        ${optionalString (p.presharedKeyFile != null) "PresharedKey = @@PEER${toString idx}@@"}
        ${optionalString (p.allowedIPs != [ ]) "AllowedIPs = ${concatStringsSep "," p.allowedIPs}"}
        ${optionalString (p.endpoint != null) "Endpoint = ${p.endpoint}"}
        PersistentKeepalive = ${toString p.persistentKeepalive}
      '';
      proxyRuleConf = r: ''
        [ProxyRule]
        CIDRs = ${concatStringsSep "," r.cidrs}
        Proxy = ${r.proxy}
        SNI = ${lib.boolToString r.sni}
      '';
    in
    ''
      [Interface]
      ${concatStringsSep "\n" (map (a: "Address = ${a}") t.address)}
      ${optionalString (t.listenPort != null) "ListenPort = ${toString t.listenPort}"}
      ${optionalString (t.privateKey != null) "PrivateKey = ${t.privateKey}"}
      ${optionalString (t.privateKeyFile != null) "PrivateKey = @@IFACE@@"}
      ${optionalString (t.mtu != null) "MTU = ${toString t.mtu}"}
      ${optionalString (t.dns != [ ]) "DNS = ${concatStringsSep "," t.dns}"}
      Table = ${t.table}
      Mode = ${t.mode}
      ${optionalString t.nat "NAT = true"}
    '' + concatStringsSep "\n" (imap1 peerConf t.peers)
    + optionalString (t.proxyRules != [ ]) "\n"
    + concatStringsSep "\n" (map proxyRuleConf t.proxyRules);

  hasFileSecrets = t:
    t.privateKeyFile != null || lib.any (p: p.presharedKeyFile != null) t.peers;

  startScript = n: t:
    pkgs.writeShellScript "ripws-up-${n}" (''
      set -eu
      psk_of() { head -n1 "$1" | tr -d '\r\n'; }
      PATH=${lib.makeBinPath ([ pkgs.coreutils pkgs.iproute2 ] ++ t.extraPackages)}:$PATH
      umask 077
      mkdir -p -m 0700 /run/ripws
      cfg=${t.runtimeConfigPath}
      cp ${pkgs.writeText "ripws-${n}.base.conf" (baseConf t)} "$cfg"
    '' + optionalString (t.privateKeyFile != null) ''
      sed -i "s|@@IFACE@@|$(psk_of ${lib.escapeShellArg t.privateKeyFile})|" "$cfg"
    '' + concatStringsSep "\n" (imap1
      (idx: p: optionalString (p.presharedKeyFile != null) ''
        sed -i "s|@@PEER${toString idx}@@|$(psk_of ${lib.escapeShellArg p.presharedKeyFile})|" "$cfg"
      '') t.peers)
    + optionalString (hasFileSecrets t) ''
      grep -q '@@' "$cfg" && { echo "ripws: unresolved secret placeholder in $cfg" >&2; exit 1; }
    '' + ''
      chmod 0600 "$cfg"
      exec ${t.package}/bin/ripws up "$cfg"
    '');
in
{
  options = {
    services.ripws = {
      package = mkPackageOption pkgs "ripws" { default = [ "ripws" ]; };

      tunnels = mkOption {
        type = types.attrsOf (types.submodule tunnelOpts);
        default = { };
        example = literalExpression ''
          {
            server = {
              address = [ "10.7.0.1/24" ];
              listenPort = 8080;
              privateKeyFile = "/run/secrets/ripws.key";
              peers = [ { uuid = "11111111-1111-4111-8111-111111111111"; allowedIPs = [ "10.7.0.2/32" ]; } ];
            };
            client = {
              address = [ "10.7.0.2/24" ];
              privateKeyFile = "/run/secrets/ripws.key";
              peers = [ {
                uuid = "11111111-1111-4111-8111-111111111111";
                endpoint = "ws://192.168.1.10:8080";
                allowedIPs = [ "0.0.0.0/0" ];
              } ];
            };
          }
        '';
        description = "ripws tunnels, keyed by interface name.";
      };
    };
  };

  config = mkIf (cfg.tunnels != { }) {
    assertions = lib.concatLists (lib.mapAttrsToList (n: t: [
      {
        assertion = t.listenPort != null
          -> (t.privateKey != null || t.privateKeyFile != null);
        message = ''
          services.ripws.tunnels.${n}: server tunnels (listenPort set)
          need privateKey or privateKeyFile.'';
      }
      {
        assertion = t.listenPort == null
          -> lib.any (p: p.endpoint != null) t.peers;
        message = ''
          services.ripws.tunnels.${n}: client tunnels need at least one
          peer with an endpoint.'';
      }
      {
        assertion = !(t.privateKey != null && t.privateKeyFile != null);
        message = ''
          services.ripws.tunnels.${n}: set only one of
          privateKey / privateKeyFile.'';
      }
    ] ++ lib.concatLists (lib.imap0 (i: r: [
      {
        assertion = r.cidrs != [ ];
        message = ''
          services.ripws.tunnels.${n}.proxyRules.${toString i}: cidrs
          must not be empty.'';
      }
      {
        # Validate the textual shape (v4: a.b.c.d/p 0..32, v6: hex
        # groups/p 0..128). The v6 branch requires a colon so v4 strings
        # cannot sneak through it.
        assertion = lib.all (c: c != ""
          && (builtins.match "([0-9]{1,3}\\.){3}[0-9]{1,3}/([0-9]|[12][0-9]|3[0-2])" c != null
              || builtins.match "[0-9A-Fa-f:]*:[0-9A-Fa-f:.]+/([0-9]{1,2}|1[01][0-9]|12[0-8])" c != null)) r.cidrs;
        message = ''
          services.ripws.tunnels.${n}.proxyRules.${toString i}: all
          cidrs must be valid IPv4/IPv6 CIDRs (e.g. "20.20.32.0/19").'';
      }
      {
        assertion = builtins.match "(https?|socks5h?)://.*" r.proxy != null;
        message = ''
          services.ripws.tunnels.${n}.proxyRules.${toString i}: proxy
          must be an http://, https://, socks5:// or socks5h:// URL.'';
      }
    ]) t.proxyRules)) cfg.tunnels);

    # /run/ripws holds the 0600 runtime configs and ripws pid files.
    systemd.tmpfiles.rules = [ "d /run/ripws 0700 root root -" ];

    systemd.services = mapAttrs' (n: t:
      nameValuePair "ripws-${n}" {
        description = "ripws tunnel ${n}";
        wantedBy = optional t.autoStart "multi-user.target";
        wants = [ "network-online.target" ];
        after = [ "network-online.target" ];

        serviceConfig = {
          Type = "simple";
          ExecStart = "${startScript n t}";
          KillSignal = "SIGTERM"; # ripws up tears the interface down cleanly
          TimeoutStopSec = 20;
          Restart = "always";
          RestartSec = 5;
        }
        // optionalAttrs (t.proxyEnvironment != { }) {
          Environment = lib.mapAttrsToList (k: v: ''${k}="${v}"'') t.proxyEnvironment;
        }
        // optionalAttrs (t.preStart != "") {
          ExecStartPre = pkgs.writeShellScript "ripws-pre-${n}" t.preStart;
        }
        // optionalAttrs (t.postStart != "") {
          ExecStartPost = pkgs.writeShellScript "ripws-post-${n}" t.postStart;
        };
      }) cfg.tunnels;
  };
}
