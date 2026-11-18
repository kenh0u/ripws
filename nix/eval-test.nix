/*
   Pure-evaluation tests for nix/module.nix (no builds, no VM).

   Run:  nix flake check          (registered as checks.<system>.ripws-eval)
         nix-build ./nix -A evalTest   (non-flake fallback)
*/
{ pkgs ? import <nixpkgs> { }, module ? ./module.nix, ripws ? pkgs.ripws }:

let
  inherit (pkgs) lib;

  psk = "SCCEuJtqD1CFE7y9f332WQ6H9o7Lr5TgKQZ4E4Q8Fp0=";
  uuid = "550e8400-e29b-41d4-a716-446655440000";
  uuidBad = "not-a-uuid";
  pskBad = "short";

  # check=false: we do not want the full NixOS system assertions here,
  # only our module's option wiring (start scripts are still built).
  eval = modules: lib.evalModules {
    modules = [
      { _module.check = false; }
      module
      { services.ripws.package = ripws; }
    ] ++ modules;
    specialArgs = { inherit pkgs; };
  };

  # Full option-type + assertion checking, for the negative tests
  # (each wrapped in builtins.tryEval).
  evalChecked = modules: lib.evalModules {
    modules = [ module { services.ripws.package = ripws; } ] ++ modules;
    specialArgs = { inherit pkgs; };
  };

  baseServer = {
    services.ripws.tunnels.s0 = {
      address = [ "10.7.0.1/24" ];
      listenPort = 8080;
      privateKey = psk;
      peers = [ { inherit uuid; allowedIPs = [ "10.7.0.2/32" ]; } ];
    };
  };

  execOf = e: n: builtins.readFile e.config.systemd.services."ripws-${n}".serviceConfig.ExecStart;

  tests = {
    # --- happy path: option wiring -------------------------------------------
    serverTunnelUnit = {
      name = "server tunnel unit";
      ok =
        let e = eval [ baseServer ];
            u = e.config.systemd.services.ripws-s0;
        in
          u.description == "ripws tunnel s0"
          && u.serviceConfig.Type == "simple"
          && u.serviceConfig.KillSignal == "SIGTERM"
          && u.wantedBy == [ "multi-user.target" ]
          && u.after == [ "network-online.target" ];
    };

    startScriptRendersConfig = {
      name = "start script invokes ripws up with runtime path";
      ok =
        let s = execOf (eval [ baseServer ]) "s0";
        in
          lib.strings.hasInfix ''ripws up "$cfg"'' s
          && lib.strings.hasInfix "cfg=/run/ripws/s0.conf" s
          && lib.strings.hasInfix "Address = 10.7.0.1/24" s
          && lib.strings.hasInfix "ListenPort = 8080" s
          && lib.strings.hasInfix "PrivateKey = ${psk}" s
          && lib.strings.hasInfix "UUID = ${uuid}" s
          && lib.strings.hasInfix "AllowedIPs = 10.7.0.2/32" s
          && lib.strings.hasInfix "PersistentKeepalive = 30" s
          # no secret-substitution machinery when everything is inline
          && !(lib.strings.hasInfix "@@" s);
    };

    privateKeyFilePlaceholder = {
      name = "privateKeyFile renders placeholder + sed";
      ok =
        let e = eval [ {
              services.ripws.tunnels.c0 = {
                address = [ "10.7.0.2/24" ];
                privateKeyFile = "/run/secrets/k";
                peers = [ { inherit uuid; endpoint = "ws://h:8080"; } ];
              };
            } ];
            s = execOf e "c0";
        in
          lib.strings.hasInfix "PrivateKey = @@IFACE@@" s
          && lib.strings.hasInfix ''sed -i "s|@@IFACE@@|$(psk_of /run/secrets/k)|"'' s
          && lib.strings.hasInfix "grep -q '@@'" s;
    };

    peerPskFilePlaceholder = {
      name = "per-peer presharedKeyFile placeholder";
      ok =
        let e = eval [ {
              services.ripws.tunnels.s0 = {
                address = [ "10.7.0.1/24" ];
                listenPort = 8080;
                privateKey = psk;
                peers = [
                  { inherit uuid; presharedKeyFile = "/run/secrets/p1"; }
                  { uuid = "6ba7b810-9dad-41d4-80b4-00c04fd430c8"; }
                ];
              };
            } ];
            s = execOf e "s0";
        in
          lib.strings.hasInfix "PresharedKey = @@PEER1@@" s
          && lib.strings.hasInfix ''sed -i "s|@@PEER1@@|$(psk_of /run/secrets/p1)|"'' s
          && !(lib.strings.hasInfix "@@PEER2@@" s);
    };

    userspaceNoDeviceFiles = {
      name = "userspace mode has no DeviceFiles";
      ok =
        let e = eval [ {
              services.ripws.tunnels.u0 = {
                mode = "userspace";
                nat = true;
                address = [ "10.9.0.1/24" ];
                listenPort = 8081;
                privateKey = psk;
                peers = [ { inherit uuid; } ];
              };
            } ];
            u = e.config.systemd.services.ripws-u0;
            s = builtins.readFile u.serviceConfig.ExecStart;
        in
          !(u.serviceConfig ? DeviceFiles)
          && lib.strings.hasInfix "Mode = userspace" s
          && lib.strings.hasInfix "NAT = true" s;
    };

    autoStartFalse = {
      name = "autoStart false detaches from boot";
      ok =
        let e = eval [ baseServer {
              services.ripws.tunnels.s0.autoStart = false;
            } ];
        in e.config.systemd.services.ripws-s0.wantedBy == [ ];
    };

    proxyEnvironmentRendered = {
      name = "proxyEnvironment reaches unit Environment";
      ok =
        let e = eval [ {
              services.ripws.tunnels.c0 = {
                address = [ "10.7.0.2/24" ];
                privateKey = psk;
                peers = [ { inherit uuid; endpoint = "ws://h:8080"; } ];
                proxyEnvironment = { ALL_PROXY = "socks5h://p:1080"; NO_PROXY = "localhost"; };
              };
            } ];
            env = e.config.systemd.services.ripws-c0.serviceConfig.Environment;
        in lib.elem ''ALL_PROXY="socks5h://p:1080"'' env
          && lib.elem ''NO_PROXY="localhost"'' env;
    };

    prePostStartHooks = {
      name = "pre/postStart hook scripts";
      ok =
        let e = eval [ baseServer {
              services.ripws.tunnels.s0.preStart = "echo hi";
              services.ripws.tunnels.s0.postStart = "ip link show s0";
            } ];
            u = e.config.systemd.services.ripws-s0;
        in
          lib.strings.hasInfix "echo hi" (builtins.readFile u.serviceConfig.ExecStartPre)
          && lib.strings.hasInfix "ip link show s0" (builtins.readFile u.serviceConfig.ExecStartPost);
    };

    tmpfilesRule = {
      name = "tmpfiles rule for /run/ripws";
      ok =
        let e = eval [ baseServer ];
        in lib.elem "d /run/ripws 0700 root root -" e.config.systemd.tmpfiles.rules;
    };

    # --- assertions (bad configs must fail) ----------------------------------
    goodConfigPasses = {
      name = "good config passes checked eval";
      ok = (builtins.tryEval (evalChecked [ baseServer ])).success;
    };

    serverWithoutKeyFails = {
      name = "server without key fails";
      ok = !(builtins.tryEval (evalChecked [ {
        services.ripws.tunnels.s0 = {
          address = [ "10.7.0.1/24" ];
          listenPort = 8080;
          peers = [ { inherit uuid; } ];
        };
      } ])).success;
    };

    clientWithoutEndpointFails = {
      name = "client without endpoint fails";
      ok = !(builtins.tryEval (evalChecked [ {
        services.ripws.tunnels.c0 = {
          address = [ "10.7.0.2/24" ];
          privateKey = psk;
          peers = [ { inherit uuid; } ];
        };
      } ])).success;
    };

    bothKeyFormsFails = {
      name = "both privateKey and privateKeyFile fails";
      ok = !(builtins.tryEval (evalChecked [ {
        services.ripws.tunnels.s0 = {
          address = [ "10.7.0.1/24" ];
          listenPort = 8080;
          privateKey = psk;
          privateKeyFile = "/run/secrets/k";
          peers = [ { inherit uuid; } ];
        };
      } ])).success;
    };

    badUuidFails = {
      name = "bad uuid rejected by type";
      ok = !(builtins.tryEval (evalChecked [ {
        services.ripws.tunnels.s0 = {
          address = [ "10.7.0.1/24" ];
          listenPort = 8080;
          privateKey = psk;
          peers = [ { uuid = uuidBad; } ];
        };
      } ])).success;
    };

    badPskFails = {
      name = "bad psk rejected by type";
      ok = !(builtins.tryEval (evalChecked [ {
        services.ripws.tunnels.s0 = {
          address = [ "10.7.0.1/24" ];
          listenPort = 8080;
          privateKey = pskBad;
          peers = [ ];
        };
      } ])).success;
    };

    emptyTunnelsNoServices = {
      name = "empty tunnels define no services";
      ok = (eval [ ]).config.systemd.services == { };
    };
  };

  failures = lib.runTests tests; # [] when everything passed
in
assert failures == [ ];
pkgs.runCommand "ripws-eval-test" { passthru.failures = failures; } "touch $out"
