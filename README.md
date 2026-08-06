# RIPWS — Raw IP over WebSocket

A minimal, Tailscale-inspired implementation of the RIPWS protocol (see
[`SPEC.md`](SPEC.md)): raw L3 packets tunneled over a WebSocket binary stream.

* Single binary, `server` / `client` subcommands.
* **Silent Drop** — unknown UUID or bad PSK ⇒ immediate TCP RST
  (`SO_LINGER=0`), no HTTP response.
* Two L3 backends behind one `tunnel.Interface`:
  * `kernel` — host TUN device (`songgao/water`), needs `CAP_NET_ADMIN` +
    `/dev/net/tun`.
  * `userspace` — in-process gVisor netstack, no privileges, no TUN.
* Bidirectional goroutine proxy pipe (`TUN ↔ WS`) with a `sync.Pool` byte
  buffer, IP-version nibble validation, and graceful packet-drop on transient
  TUN write faults.
* WebSocket Ping/Pong keep-alive with a jittered, spec-compliant 30–300 s
  client interval and a server read-window ≥ slowest legal ping.
* Exponential backoff reconnection with full jitter.

## Build (Nix flake)

```sh
nix build .#ripws          # -> result/bin/ripws (static Go binary)
nix develop                # shell with go / gopls for hacking
```

`flake.nix` uses `buildGoModule`; the gVisor/netstack version is pinned in
`go.mod` (one `go get` away from bumping, with the vendor hash re-computed by
`nix build`).

## Usage

```sh
# Mint credentials
ripws gen-psk        # -> 44-char base64 (32 random bytes)
ripws gen-config     # -> {"<uuidv4>": "<psk>"}

# Server (kernel TUN)
sudo ripws server --listen :8080 --config creds.json --mode kernel
# then: sudo ip addr add 10.7.0.1/24 dev tun0 && sudo ip link set up dev tun0

# Client (kernel TUN)
sudo ripws client --url ws://SERVER:8080/<uuid> --psk <psk> \
                  --mode kernel --ping-interval 30s
# then: sudo ip addr add 10.7.0.2/24 dev tun0 && sudo ip link set up dev tun0

# Userspace (no root, no TUN)
ripws server --listen :8080 --uuid <uuid> --psk <psk> \
             --mode userspace --us-addr 10.8.0.1 --us-echo 10.8.0.1:7
ripws client --url ws://SERVER:8080/<uuid> --psk <psk> \
             --mode userspace --us-addr 10.8.0.2 --us-route 10.8.0.0/24 \
             --us-dial 10.8.0.1:7
```

Flags are documented in `ripws help`.

## Docker test (L3 ping, kernel mode)

`scripts/test.sh` builds the binary with Nix, bakes it into a Debian image, and
brings up two containers on a bridge network:

```
ripws-client (tun0 10.7.0.2/24)  --ws-->  ripws-server (tun0 10.7.0.1/24)
```

Both containers run with `--cap-add=NET_ADMIN --device=/dev/net/tun`. The
entrypoint (`scripts/entrypoint.sh`) starts `ripws`, waits for `tun0`, then
assigns the address and brings the link up.

```sh
bash scripts/test.sh
```

Expected: `ping -c 4 10.7.0.1` from the client and `ping -c 4 10.7.0.2` from
the server both succeed; a probe with a bad UUID/PSK yields
`Connection reset by peer` with **no HTTP status line** (Silent Drop).

### Equivalent manual commands

```sh
nix build .#ripws
mkdir -p bin && cp -L result/bin/ripws bin/ripws
sudo docker compose build

# credentials
CRED="$(nix run .#ripws -- gen-config)"
UUID="$(printf '%s' "$CRED" | jq -r 'keys[0]')"
PSK="$(printf '%s' "$CRED" | jq -r ".[\"$UUID\"]")"
mkdir -p config && printf '%s\n' "$CRED" > config/server.json

export RIPWS_UUID="$UUID" RIPWS_PSK="$PSK"
sudo -E docker compose up -d server client

# TUN setup is done automatically by the entrypoint; verify:
sudo docker exec ripws-client  ping -c 3 10.7.0.1
sudo docker exec ripws-server  ping -c 3 10.7.0.2

# Silent Drop (expect RST, no status line):
sudo docker exec ripws-client curl -i --max-time 5 \
  http://server:8080/00000000-0000-0000-0000-000000000000 \
  -H 'Authorization: Bearer bad'
```

## Docker test (userspace echo)

`scripts/test-userspace.sh` runs both sides in `--mode userspace` with gVisor
netstacks. The server hosts a TCP echo listener at `10.8.0.1:7` *inside* its
netstack; the client dials it through the tunnel every 2 s:

```sh
bash scripts/test-userspace.sh
```

Expected: `probe 10.8.0.1:7 OK rtt≈1ms` logged on the client. No
`/dev/net/tun` is used on the data path (the test still mounts it because the
image is shared with the kernel test).

## Layout

```
main.go                      flag-based server/client/gen-* dispatch
flake.nix                    buildGoModule (static binary)
internal/
  config/                    JSON + flag credentials, PSK validation (SPEC 2.2)
  proto/                     IP-version nibble, keep-alive, exponential backoff
  tunnel/                    Interface + kernel TUN + gVisor netstack backends
  pipe/                      bidirectional TUN<->WS proxy + buffer pool
  server/                    Silent Drop (hijack + SO_LINGER=0) + WS upgrade
  client/                    dial + reconnect + jittered ping
scripts/                     entrypoint.sh, test.sh, test-userspace.sh
Dockerfile, docker-compose.yml
```

## Dependencies

* `github.com/gorilla/websocket` — WebSocket (Tailscale’s choice; `http.Hijacker`
  access makes Silent Drop natural).
* `github.com/songgao/water` — Linux TUN.
* `gvisor.dev/gvisor` — userspace netstack (pinned; `go.mod` + Nix vendor hash).
* `github.com/google/uuid` — UUIDv4 validation.
