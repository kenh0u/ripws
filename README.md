# RIPWS — Raw IP over WebSocket

**[日本語版はこちら (Japanese)](README_ja.md)**

A minimal, Tailscale-inspired implementation of the RIPWS protocol (see
[`SPEC.md`](SPEC.md)): raw L3 packets tunneled over a WebSocket binary stream.

- Single binary with `up` / `down` / `server` / `client` / `gen-psk` / `gen-config` subcommands.
- **wg-quick-compatible configuration** — `ripws up ws0.conf` automatically creates the TUN device, assigns addresses, configures MTU, routes, DNS, and runs PreUp/PostUp hooks.
- **Silent Drop** — unknown UUID or bad PSK ⇒ immediate TCP RST
  (`SO_LINGER=0`), no HTTP response.
- Two L3 backends behind one `tunnel.Interface`:
  - `kernel` — host TUN device (`songgao/water`), requires `CAP_NET_ADMIN` +
    `/dev/net/tun`.
  - `userspace` — in-process gVisor netstack, **no privileges, no TUN**.
- Bidirectional goroutine proxy pipe (`TUN ↔ WS`) with a `sync.Pool` byte
  buffer, IP-version nibble validation, and graceful packet-drop on transient
  TUN write faults.
- WebSocket Ping/Pong keep-alive with a jittered, spec-compliant 30–300 s
  client interval and a server read-window ≥ slowest legal ping.
- Exponential backoff reconnection with full jitter.
- **Userspace NAT gateway** — transit traffic from the tunnel is transparently
  proxied out through the host's kernel networking via `net.Dial`. No
  `CAP_NET_ADMIN` required.

---

## Table of Contents

- [Build](#build)
- [Subcommand Overview](#subcommand-overview)
- [Quick Start (wg-quick style)](#quick-start-wg-quick-style)
- [wg-quick-compatible Config File](#wg-quick-compatible-config-file)
- [gen-psk](#gen-psk)
- [gen-config](#gen-config)
- [server](#server)
- [client](#client)
- [Usage Examples](#usage-examples)
  - [1. Kernel Mode (Basic Tunnel)](#1-kernel-mode-basic-tunnel)
  - [2. Userspace Mode (No Privileges)](#2-userspace-mode-no-privileges)
  - [3. Userspace ICMP Ping](#3-userspace-icmp-ping)
  - [4. Userspace TCP/UDP Port Forward](#4-userspace-tcpudp-port-forward)
  - [5. Userspace NAT Gateway](#5-userspace-nat-gateway)
  - [6. Cloudflare Tunnel (wss://)](#6-cloudflare-tunnel-wss)
  - [7. Multiple Clients (JSON Config File)](#7-multiple-clients-json-config-file)
  - [8. Echo Probe (Userspace Connectivity Check)](#8-echo-probe-userspace-connectivity-check)
- [Test Scripts](#test-scripts)
- [Docker Tests](#docker-tests)
- [Project Layout](#project-layout)
- [Dependencies](#dependencies)
- [Test Results Summary](#test-results-summary)

---

## Build

### Nix flake (recommended)

```sh
nix build .#ripws          # -> result/bin/ripws (static Go binary, CGO disabled)
nix develop                # shell with go / gopls for hacking
```

`flake.nix` uses `buildGoModule`; the gVisor/netstack version is pinned in
`go.mod`.

> **Note:** Nix flake builds from Git-tracked source — new files must be
> `git add`ed before `nix build`.

### Build with Go directly

```sh
go build -o ripws .
```

---

## Subcommand Overview

```
ripws up   <config.conf>  Bring up a tunnel wg-quick-style (TUN creation, addr, MTU, routes, DNS, hooks)
ripws down <config.conf>  Tear down a tunnel (remove interface, restore DNS, hooks)
ripws server   [flags]    Start the RIPWS server (manual interface setup)
ripws client   [flags]    Start the RIPWS client (manual interface setup)
ripws gen-psk               Generate a fresh 32-byte Base64 PSK
ripws gen-config            Generate JSON mapping a fresh UUID to a fresh PSK
ripws help                  Show help
```

---

## Quick Start (wg-quick style)

`ripws up` / `ripws down` use a wg-quick-compatible INI config file to fully
automate TUN creation, address assignment, MTU, route addition, DNS injection,
and PreUp/PostUp hooks. Requires `CAP_NET_ADMIN`.

```sh
# Generate credentials
ripws gen-config > /tmp/creds.json
UUID="$(jq -r 'keys[0]' /tmp/creds.json)"
PSK="$(jq -r ".[\"$UUID\"]" /tmp/creds.json)"

# --- Server config (/etc/ripws/ws0.conf) ---
cat > /tmp/ws0.conf << EOF
[Interface]
Address    = 10.7.0.1/24
ListenPort = 8080
PrivateKey = $PSK
MTU        = 1420

[Peer]
PublicKey  = $UUID
AllowedIPs = 10.7.0.2/32
EOF

# --- Client config (/etc/ripws/ws0.conf) ---
cat > /tmp/ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
MTU        = 1420

[Peer]
PublicKey  = $UUID
Endpoint   = ws://SERVER:8080/$UUID
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 30
EOF

# Bring up
sudo ripws up /tmp/ws0.conf        # Interface name auto-derived from filename (ws0.conf -> ws0)

# Bring down
sudo ripws down /tmp/ws0.conf
```

> If placed at `/etc/ripws/ws0.conf`, you can abbreviate: `ripws up ws0` /
> `ripws down ws0`.

---

## wg-quick-compatible Config File

Uses INI format with an `[Interface]` section and one or more `[Peer]` sections.

### [Interface] Section

| Field | Required | Description |
|-------|----------|-------------|
| `Address` | ✓ | CIDR address(es) assigned to the interface (comma-separated) |
| `ListenPort` | server only | Server listen port. When present, runs in server mode |
| `PrivateKey` | ✓ | Default PSK (base64, 44 chars). Overridden per-peer by `PresharedKey` |
| `MTU` | (default 1420) | Interface MTU |
| `DNS` | (optional) | DNS server(s) injected into `/etc/resolv.conf` (comma-separated) |
| `Table` | (default `auto`) | `off` skips route addition |
| `PreUp` | (optional) | Shell command(s) to run before interface comes up (multiple lines allowed) |
| `PostUp` | (optional) | Shell command(s) to run after interface comes up (multiple lines allowed) |
| `PreDown` | (optional) | Shell command(s) to run before interface goes down (multiple lines allowed) |
| `PostDown` | (optional) | Shell command(s) to run after interface goes down (multiple lines allowed) |

### [Peer] Section

| Field | Required | Description |
|-------|----------|-------------|
| `PublicKey` | ✓ | Peer's UUIDv4 |
| `PresharedKey` | (optional) | Per-peer PSK; overrides `PrivateKey` when present |
| `AllowedIPs` | ✓ | Routes via this peer (CIDR, comma-separated) |
| `Endpoint` | client only | `ws(s)://host:port/<uuid>` server URL |
| `PersistentKeepalive` | (optional) | WebSocket Ping interval in seconds (30–300) |

### WireGuard Field Mapping

| WireGuard | RIPWS | Notes |
|-----------|-------|-------|
| `PrivateKey` | PSK | Authentication token (Bearer) |
| `PublicKey` | UUID | Client identifier |
| `PresharedKey` | per-peer PSK | Per-peer PSK override |
| `Endpoint` | ws(s):// URL | WebSocket endpoint |
| `ListenPort` | listen port | Server mode detection |
| `AllowedIPs` | routes | Tunnel routes |
| `PersistentKeepalive` | Ping interval | WebSocket Ping (30–300 s) |
| `MTU`, `DNS`, `Table`, `PreUp/PostUp/PreDown/PostDown` | same name, same function | wg-quick compatible |

### Server vs Client Detection

- `ListenPort` present → **server** (starts HTTP server)
- `ListenPort` absent → **client** (connects to first `[Peer]`'s `Endpoint`)

### Interface Name Derivation

The interface name is derived from the config file's basename minus `.conf`:
- `ws0.conf` → `ws0`
- `/etc/ripws/tunnel1.conf` → `tunnel1`

---

## gen-psk

Generates a Pre-Shared Key: 32 random bytes Base64-encoded into a 44-character
string.

```sh
ripws gen-psk
# Example output: 8x/P9mZ3qF1bR6wN2vT4xK7jL0sH5aBcDeFgHiJk=
```

Options: none

---

## gen-config

Generates a fresh UUIDv4 and a corresponding PSK, output as JSON. Use as a
template for the server config file.

```sh
ripws gen-config
# Example output:
# {
#   "f47ac10b-58cc-4372-a567-0e02b2c3d479": "8x/P9mZ3qF1bR6wN2vT4xK7jL0sH5aBcDeFgHiJk="
# }
```

Options: none

---

## server

Starts the RIPWS server. After WebSocket upgrade, it acts as an L3 packet
tunnel endpoint.

### Flag Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--listen ADDR` | `:8080` | Listen address |
| `--config PATH` | (empty) | JSON file mapping UUID → PSK (multi-client support) |
| `--uuid UUID` | (empty) | Single-client UUID (can be combined with `--config`) |
| `--psk PSK` | (empty) | Single-client PSK |
| `--mode kernel\|userspace` | `kernel` | L3 backend selection |
| `--tun-name NAME` | (empty) | kernel: desired TUN name hint (best effort) |
| `--us-addr IP[,IP...]` | (empty) | userspace: virtual IPs assigned to the netstack NIC (comma-separated) |
| `--us-route CIDR[,CIDR..]` | `0.0.0.0/0,::/0` | userspace: routes via the tunnel NIC (comma-separated CIDRs) |
| `--us-echo ADDR` | (empty) | userspace: start a TCP echo listener inside the netstack (e.g. `10.8.0.1:7`) |
| `--ping-window D` | `330s` | Read-deadline window for reaping dead clients (≥ slowest client ping 300s) |

> `--config` and `--uuid`/`--psk` can be combined. When both are specified, the
> flag credentials are merged in. At least one credential is required.

### Config File Format

```json
{
  "client-a-uuid": "client-a-psk-base64",
  "client-b-uuid": "client-b-psk-base64"
}
```

---

## client

Starts the RIPWS client. Connects to the server via WebSocket and bridges L3
packets bidirectionally. On disconnect, reconnects with exponential backoff +
jitter.

### Flag Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--url URL` | (required) | `ws(s)://host:port/<uuid>` endpoint |
| `--psk PSK` | (required) | Pre-Shared Key (Bearer token) |
| `--mode kernel\|userspace` | `kernel` | L3 backend selection |
| `--tun-name NAME` | (empty) | kernel: TUN name hint |
| `--tun-addr ADDR` | (empty) | kernel: informational; address to assign to the TUN device (printed at startup) |
| `--us-addr IP[,IP...]` | (empty) | userspace: local virtual IP(s) (comma-separated) |
| `--us-route CIDR[,CIDR..]` | `0.0.0.0/0,::/0` | userspace: routes via tunnel NIC |
| `--us-dial ADDR` | (empty) | userspace: one-shot echo probe target (e.g. `10.8.0.1:7`) |
| `--us-dial-interval D` | `5s` | echo probe repeat interval |
| `--us-ping IP` | (empty) | userspace: ICMP ping destination IP (e.g. `10.8.0.1`) |
| `--us-ping-count N` | `4` | number of ping probes |
| `--us-ping-interval D` | `1s` | interval between ping probes |
| `--us-forward L=R` | (empty) | userspace: TCP port forward `LOCAL=REMOTE` (e.g. `127.0.0.1:5201=10.8.0.1:5201`) |
| `--us-udp-forward L=R` | (empty) | userspace: UDP port forward `LOCAL=REMOTE` |
| `--us-nat` | `false` | userspace: enable NAT gateway (tunnel transit traffic proxied out via host networking) |
| `--ping-interval D` | `60s` | WebSocket Ping interval (must be within 30s–300s) |
| `--ping-pong-timeout D` | `15s` | Pong wait before declaring connection dead |
| `--backoff-base D` | `500ms` | initial reconnect delay |
| `--backoff-max D` | `60s` | reconnect delay cap |
| `--max-retries N` | `0` | max reconnect attempts (0 = unlimited) |

---

## Usage Examples

### 1. Kernel Mode (Basic Tunnel)

The most basic configuration. Both server and client use TUN devices. Requires
`CAP_NET_ADMIN` and `/dev/net/tun`.

```sh
# --- Server ---
sudo ripws server --listen :8080 --config creds.json --mode kernel
# After startup, assign an address to the TUN device:
sudo ip addr add 10.7.0.1/24 dev tun0 && sudo ip link set up dev tun0

# --- Client ---
sudo ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode kernel \
    --ping-interval 30s
# After startup, assign an address to the TUN device:
sudo ip addr add 10.7.0.2/24 dev tun0 && sudo ip link set up dev tun0

# Verify connectivity
ping -c 4 10.7.0.1   # client → server
ping -c 4 10.7.0.2   # server → client
```

You can also specify a single client directly via flags:

```sh
sudo ripws server --listen :8080 --uuid <uuid> --psk <psk> --mode kernel
```

---

### 2. Userspace Mode (No Privileges)

Uses gVisor netstack. No root, no TUN device required. Available on both server
and client.

```sh
# --- Server (userspace + echo listener) ---
ripws server \
    --listen :8080 \
    --uuid <uuid> --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.1 \
    --us-echo 10.8.0.1:7

# --- Client (userspace) ---
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.2 \
    --us-route 10.8.0.0/24
```

> Setting `--us-route` to `0.0.0.0/0` (the default) routes all traffic through
> the tunnel. To tunnel only a specific subnet, specify e.g.
> `--us-route 10.8.0.0/24`.

---

### 3. Userspace ICMP Ping

Uses gVisor's ICMP endpoint to send pings through the tunnel. No privileges
required.

```sh
# Server (assuming kernel TUN 10.8.0.1/24 is running)

# Client
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.2 \
    --us-route 10.8.0.0/24 \
    --us-ping 10.8.0.1 \
    --us-ping-count 10 \
    --us-ping-interval 1s
```

Example output:
```
client: PING 10.8.0.1 (10 probes, 1s interval)
client: ping seq=0 rtt=689.324µs
client: ping seq=1 rtt=451.212µs
...
client: ping 10.8.0.1: 10 sent, 10 recv, 0% loss, min=312µs avg=690µs max=1.08ms
```

---

### 4. Userspace TCP/UDP Port Forward

Forwards a local listener port to a remote address through the tunnel. Allows
tools like iperf3 to be used over the tunnel.

```sh
# Server (assuming kernel TUN 10.8.0.1/24, iperf3 -s -B 10.8.0.1 is running)

# Client — TCP + UDP forward
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.2 \
    --us-route 10.8.0.0/24 \
    --us-forward 127.0.0.1:5201=10.8.0.1:5201 \
    --us-udp-forward 127.0.0.1:5201=10.8.0.1:5201 \
    --us-ping 10.8.0.1 --us-ping-count 10

# From another terminal, run iperf3
iperf3 -c 127.0.0.1 -p 5201 -t 10          # TCP c2s
iperf3 -c 127.0.0.1 -p 5201 -t 10 -R       # TCP s2c (reverse)
iperf3 -c 127.0.0.1 -p 5201 -t 10 -P 8     # TCP 8 parallel
iperf3 -c 127.0.0.1 -p 5201 -t 10 -u -b 200M   # UDP 200Mbps
```

---

### 5. Userspace NAT Gateway

Runs the client as a NAT gateway, forwarding transit traffic from the server to
the internet via the client. No privileges required on the client side.

```
Server (kernel TUN 10.7.0.1/24)
    │  ip route add 1.1.1.1/32 8.8.8.8/32 via 10.7.0.2
    │  WebSocket ws://server:8080/<uuid>
Client (userspace netstack 10.7.0.2, --us-nat)
    │  gVisor TCP/UDP Forwarder → net.Dial → Internet
Internet (1.1.1.1, 8.8.8.8, ...)
```

```sh
# --- Client (NAT gateway, no privileges) ---
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.7.0.2 \
    --us-route 10.7.0.0/24 \
    --us-nat

# --- Server (kernel TUN 10.7.0.1/24) ---
# Route external IPs via the client
sudo ip route add 1.1.1.1/32 via 10.7.0.2
sudo ip route add 8.8.8.8/32 via 10.7.0.2

# From server to internet (tunnel → client NAT → internet)
curl http://1.1.1.1/                          # TCP 80
curl https://1.1.1.1/                         # TCP 443 (TLS)
dig @8.8.8.8 example.com                      # UDP 53 (DNS)
curl http://example.com/                      # TCP (DNS + HTTP)
```

> The NAT gateway uses gVisor's TCP/UDP Forwarders to receive packets and
> bridges them to the host's kernel networking via `net.Dial`. No
> `CAP_NET_ADMIN` is needed on the client.

---

### 6. Cloudflare Tunnel (wss://)

Uses Cloudflare Tunnel (`cloudflared`) as a reverse proxy. TLS encryption + CDN
traversal via `wss://`.

```sh
# --- Server (Docker container, kernel TUN 10.7.0.1/24) ---
# Cloudflare Tunnel reverse-proxies localhost:8080
sudo ripws server --listen :8080 --config creds.json --mode kernel
sudo ip addr add 10.7.0.1/24 dev tun0 && sudo ip link set up dev tun0
# Bind iperf3 to the tunnel IP
iperf3 -s -p 5201 -B 10.7.0.1

# --- cloudflared (running on the host) ---
cloudflared tunnel --no-autoupdate --protocol http2 run --token <CF_TOKEN>
# --protocol http2 is required (QUIC datagram handler fails in some environments)

# --- Client (on host, userspace mode) ---
sudo ripws client \
    --url "wss://ripws-test.example.com/<uuid>" \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.7.0.2 \
    --us-route 10.7.0.0/24 \
    --us-ping 10.7.0.1 --us-ping-count 100 --us-ping-interval 0.1s \
    --us-forward 127.0.0.1:5201=10.7.0.1:5201 \
    --us-udp-forward 127.0.0.1:5201=10.7.0.1:5201 \
    --ping-interval 30s

# iperf3 (through Cloudflare)
iperf3 -c 127.0.0.1 -p 5201 -t 15 -P 8       # TCP 8 parallel
iperf3 -c 127.0.0.1 -p 5201 -t 15 -u -b 500M  # UDP 500Mbps
```

Kernel mode through Cloudflare also works:

```sh
sudo ripws client \
    --url "wss://ripws-test.example.com/<uuid>" \
    --psk <psk> \
    --mode kernel \
    --ping-interval 30s
sudo ip addr add 10.7.0.2/24 dev tun0 && sudo ip link set up dev tun0
iperf3 -c 10.7.0.1 -p 5201 -B 10.7.0.2
```

> **Silent Drop + Cloudflare:** Probes with bad UUID/PSK are RST'd at the
> origin, but Cloudflare translates this to HTTP 502 for the client. The origin
> itself returns no HTTP response.

---

### 7. Multiple Clients (JSON Config File)

```sh
# Manage multiple UUID-PSK pairs in a JSON file
ripws gen-config > creds.json
# Manually add entries to creds.json:
# {
#   "uuid-a": "psk-a",
#   "uuid-b": "psk-b",
#   "uuid-c": "psk-c"
# }

# One server accepts multiple clients
sudo ripws server --listen :8080 --config creds.json --mode kernel

# Each client connects with its own UUID/PSK
sudo ripws client --url ws://SERVER:8080/uuid-a --psk psk-a --mode kernel
sudo ripws client --url ws://SERVER:8080/uuid-b --psk psk-b --mode kernel
```

Combining `--config` with `--uuid`/`--psk`:

```sh
# JSON file + additional flag credential
sudo ripws server --config creds.json --uuid <extra-uuid> --psk <extra-psk>
```

---

### 8. Echo Probe (Userspace Connectivity Check)

Starts a TCP echo listener inside the server's gVisor netstack and probes it
periodically from the client:

```sh
# Server (userspace, with echo listener)
ripws server \
    --listen :8080 \
    --uuid <uuid> --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.1 \
    --us-echo 10.8.0.1:7

# Client (userspace, periodic probe)
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.2 \
    --us-route 10.8.0.0/24 \
    --us-dial 10.8.0.1:7 \
    --us-dial-interval 2s
```

Example output:
```
client: probe 10.8.0.1:7 OK rtt=1.2ms
client: probe 10.8.0.1:7 OK rtt=0.9ms
```

---

## Test Scripts

| Script | Description |
|--------|-------------|
| `scripts/test.sh` | Kernel mode L3 ping + Silent Drop (Docker, 2 containers) |
| `scripts/test-userspace.sh` | Userspace echo probe (both sides gVisor) |
| `scripts/test-us-direct.sh` | Userspace direct benchmark (client userspace + server kernel, iperf3 + ping) |
| `scripts/test-nat.sh` | NAT gateway test (server → tunnel → client NAT → internet, TCP/UDP/DNS) |
| `scripts/cloudflare-load-test.sh` | Cloudflare kernel mode load test (iperf3 + 1000 pings) |
| `scripts/cloudflare-us-test.sh` | Cloudflare userspace test (iperf3 + 100 pings) |

---

## Docker Tests

### Kernel Mode (L3 ping, Silent Drop)

```
ripws-client (tun0 10.7.0.2/24)  --ws-->  ripws-server (tun0 10.7.0.1/24)
```

Both containers run with `--cap-add=NET_ADMIN --device=/dev/net/tun`. The
entrypoint (`scripts/entrypoint.sh`) starts `ripws`, waits for `tun0`, then
assigns the address and brings the link up.

```sh
bash scripts/test.sh
```

Expected: `ping -c 4 10.7.0.1` (client→server) and `ping -c 4 10.7.0.2`
(server→client) both succeed; a probe with bad UUID/PSK yields
`Connection reset by peer` with **no HTTP status line** (Silent Drop).

### Userspace Direct Benchmark

```
ripws-client (userspace 10.8.0.2)  --ws-->  ripws-server (tun0 10.8.0.1/24, iperf3 -B 10.8.0.1)
```

```sh
bash scripts/test-us-direct.sh
```

### NAT Gateway

```
ripws-server (tun0 10.7.0.1/24) → tunnel → ripws-client (userspace 10.7.0.2, --us-nat) → internet
```

```sh
bash scripts/test-nat.sh
```

### Cloudflare

```sh
CF_TUNNEL_TOKEN=<token> bash scripts/cloudflare-load-test.sh    # kernel mode
CF_TUNNEL_TOKEN=<token> bash scripts/cloudflare-us-test.sh      # userspace mode
```

### Manual Docker Commands

```sh
nix build .#ripws
mkdir -p bin && cp -L result/bin/ripws bin/ripws
sudo docker compose build

# Generate credentials
CRED="$(nix run .#ripws -- gen-config)"
UUID="$(printf '%s' "$CRED" | jq -r 'keys[0]')"
PSK="$(printf '%s' "$CRED" | jq -r ".[\"$UUID\"]")"
mkdir -p config && printf '%s\n' "$CRED" > config/server.json

export RIPWS_UUID="$UUID" RIPWS_PSK="$PSK"
sudo -E docker compose up -d server client

# Verify
sudo docker exec ripws-client ping -c 3 10.7.0.1
sudo docker exec ripws-server ping -c 3 10.7.0.2

# Silent Drop (expect RST, no status line)
sudo docker exec ripws-client curl -i --max-time 5 \
  http://server:8080/00000000-0000-0000-0000-000000000000 \
  -H 'Authorization: Bearer bad'
```

---

## Project Layout

```
main.go                      flag-based up/down/server/client/gen-* dispatch
flake.nix                    buildGoModule (static binary)
internal/
  config/                    JSON + flag credentials, PSK validation (SPEC 2.2)
    quickconf.go             wg-quick-compatible INI parser
  proto/                     IP-version nibble, keep-alive, exponential backoff
  tunnel/
    tunnel.go                Interface abstract contract
    tun_linux.go             kernel TUN backend (songgao/water)
    netstack.go              gVisor userspace backend (netstack)
    setup_linux.go           wg-quick-style ifup/ifdown (addr/MTU/routes/DNS/hooks)
    ping.go                  userspace ICMP ping (gVisor ICMP endpoint)
    forward.go               userspace TCP/UDP port forwarder
    nat.go                   userspace NAT gateway (gVisor Forwarder → net.Dial)
  pipe/                      bidirectional TUN↔WS proxy + buffer pool
  server/                    Silent Drop (hijack + SO_LINGER=0) + WS upgrade
  client/                    dial + reconnect + jittered ping
scripts/                     entrypoint.sh, test.sh, test-*.sh, cloudflare-*.sh
Dockerfile, docker-compose*.yml
```

---

## Dependencies

- `github.com/gorilla/websocket` — WebSocket (`http.Hijacker` access makes
  Silent Drop natural to implement).
- `github.com/songgao/water` — Linux TUN.
- `gvisor.dev/gvisor` — userspace netstack (pinned; `go.mod` + Nix vendor hash).
- `github.com/google/uuid` — UUIDv4 generation and validation.

---

## Test Results Summary

See [`REPORT.md`](REPORT.md) for full test results.

| Category | Result |
|----------|--------|
| L3 connectivity (kernel) | PASS |
| Silent Drop security | PASS |
| Userspace (gVisor) basic | PASS |
| iperf3 throughput (kernel) | PASS (1.6+ Gbps) |
| Multi-port concurrency | PASS (32/32) |
| Reconnection & KeepAlive | PASS |
| Race detector | PASS |
| Cloudflare tunnel (kernel) | PASS (wss://, bidirectional, RTT ~232ms) |
| Userspace ping + iperf (direct) | PASS (0% loss, 2.0 Gbps -P8) |
| Userspace NAT gateway | PASS (TCP/UDP/DNS, tunnel → internet) |
| Cloudflare tunnel (userspace) | PASS (wss://, 0% ping loss, UDP 499 Mbps 0% loss) |
