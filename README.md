# RIPWS — Raw IP over WebSocket

**[日本語版はこちら (Japanese)](README_ja.md)**

A minimal, Tailscale-inspired implementation of the RIPWS protocol (see
[`SPEC.md`](SPEC.md)): raw L3 packets tunneled over a WebSocket binary stream.

- Single binary with `up` / `down` / `genkey` / `genuuid` subcommands.
- **wg-quick-compatible configuration** — `ripws up ws0.conf` automatically creates the TUN device, assigns addresses, configures MTU, routes, DNS, and runs PreUp/PostUp hooks.
- **Silent Drop** — unknown UUID or bad PSK ⇒ immediate TCP RST
  (`SO_LINGER=0`), no HTTP response.
- Two L3 backends behind one `tunnel.Interface`:
  - `kernel` — host TUN device (`songgao/water`), requires `CAP_NET_ADMIN` +
    `/dev/net/tun`. (wg-quick-respect)
  - `userspace` — in-process gVisor netstack, **no privileges, no TUN**. (Tailscale-respect)
- **Userspace NAT gateway** — transit traffic from the tunnel is transparently
  proxied out through the host's kernel networking via `net.Dial`. No
  `CAP_NET_ADMIN` required.
- Bidirectional goroutine proxy pipe (`TUN ↔ WS`) with a `sync.Pool` byte
  buffer, IP-version nibble validation, and graceful packet-drop on transient
  TUN write faults.
- WebSocket Ping/Pong keep-alive with a jittered, spec-compliant 30–300 s
  client interval and a server read-window ≥ slowest legal ping.
- Exponential backoff reconnection with full jitter.

---

## Table of Contents

- [Build](#build)
- [Subcommand Overview](#subcommand-overview)
- [Quick Start](#quick-start)
- [Config File Format](#config-file-format)
- [genkey](#genkey)
- [genuuid](#genuuid)
- [Usage Examples](#usage-examples)
  - [1. Kernel Mode (Basic Tunnel)](#1-kernel-mode-basic-tunnel)
  - [2. Userspace Mode (No Privileges)](#2-userspace-mode-no-privileges)
  - [3. Userspace NAT Gateway](#3-userspace-nat-gateway)
  - [4. Cloudflare Tunnel (wss://)](#4-cloudflare-tunnel-wss)
  - [5. Multiple Clients (JSON Config File)](#5-multiple-clients-json-config-file)
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

> **Note:** Nix flake builds from Git-tracked source — changes must be
> committed before `nix build` picks them up.

### Build with Go directly

```sh
CGO_ENABLED=0 go build -o ripws .
```

---

## Subcommand Overview

```
ripws up   <config.conf>  Bring up a tunnel (TUN/netstack creation, addr, MTU, routes, DNS, hooks)
ripws down <iface|conf>   Tear down a running tunnel (SIGTERM + cleanup)
ripws genkey             Generate a fresh 32-byte Base64 PSK (WireGuard-style)
ripws genuuid            Generate a fresh UUIDv4 (client identity)
ripws help                Show help
```

---

## Quick Start

`ripws up` / `ripws down` use a wg-quick-compatible INI config file to fully
automate interface creation, address assignment, MTU, route addition, DNS
injection, and PreUp/PostUp hooks.

```sh
# Generate credentials
UUID="$(ripws genuuid)"
PSK="$(ripws genkey)"

# --- Server config (/etc/ripws/ws0.conf) ---
cat > /tmp/ws0.conf << EOF
[Interface]
Address    = 10.7.0.1/24
ListenPort = 8080
PrivateKey = $PSK
MTU        = 1420

[Peer]
UUID  = $UUID
AllowedIPs = 10.7.0.2/32
EOF

# --- Client config (/etc/ripws/ws0.conf) ---
cat > /tmp/ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = ws://SERVER:8080
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

## Config File Format

Uses INI format with an `[Interface]` section and one or more `[Peer]` sections.

### [Interface] Section

| Field | Required | Description |
|-------|----------|-------------|
| `Address` | ✓ | CIDR address(es) assigned to the interface (comma-separated) |
| `ListenPort` | server only | Server listen port. When present, runs in server mode |
| `PrivateKey` | ✓ | Default PSK (base64, 44 chars). Overridden per-peer by `PresharedKey` |
| `MTU` | (default 1420) | Interface MTU |
| `Mode` | (default `kernel`) | `kernel` (TUN, requires CAP_NET_ADMIN) or `userspace` (gVisor netstack, no privileges) |
| `NAT` | (default `false`) | Userspace only: enable NAT gateway (tunnel transit → host networking) |
| `DNS` | (optional) | DNS server(s) injected into `/etc/resolv.conf` (comma-separated) |
| `Table` | (default `auto`) | `off` skips route addition |
| `PreUp` | (optional) | Shell command(s) to run before interface comes up (multiple lines allowed) |
| `PostUp` | (optional) | Shell command(s) to run after interface comes up (multiple lines allowed) |
| `PreDown` | (optional) | Shell command(s) to run before interface goes down (multiple lines allowed) |
| `PostDown` | (optional) | Shell command(s) to run after interface goes down (multiple lines allowed) |

### [Peer] Section

| Field | Required | Description |
|-------|----------|-------------|
| `UUID` | ✓ | Peer's UUIDv4 |
| `PresharedKey` | (optional) | Per-peer PSK; overrides `PrivateKey` when present |
| `AllowedIPs` | ✓ | Routes via this peer (CIDR, comma-separated) |
| `Endpoint` | client only | `ws(s)://host:port` server URL (UUID appended automatically) |
| `PersistentKeepalive` | (optional) | WebSocket Ping interval in seconds (30–300) |

### WireGuard Field Mapping

| WireGuard | RIPWS | Notes |
|-----------|-------|-------|
| `PrivateKey` | PSK | Authentication token (Bearer) |
| `PublicKey` | `UUID` | Client identifier (not a public key; renamed for clarity) |
| `PresharedKey` | per-peer PSK | Per-peer PSK override |
| `Endpoint` | ws(s):// URL | WebSocket endpoint |
| `ListenPort` | listen port | Server mode detection |
| `AllowedIPs` | routes | Tunnel routes |
| `PersistentKeepalive` | Ping interval | WebSocket Ping (30–300 s) |
| `MTU`, `DNS`, `Table`, `PreUp/PostUp/PreDown/PostDown` | same name, same function | wg-quick compatible |

### Server vs Client Detection

- `ListenPort` present → **server** (starts HTTP server)
- `ListenPort` absent → **client** (connects to first `[Peer]`'s `Endpoint`)

### Kernel vs Userspace Mode

- `Mode = kernel` (default) → uses host TUN device, requires `CAP_NET_ADMIN` + `/dev/net/tun`
- `Mode = userspace` → uses in-process gVisor netstack, no privileges needed

### Interface Name Derivation

The interface name is derived from the config file's basename minus `.conf`:
- `ws0.conf` → `ws0`
- `/etc/ripws/tunnel1.conf` → `tunnel1`

---

## genkey

Generates a Pre-Shared Key: 32 random bytes Base64-encoded into a 44-character
string. WireGuard-style naming (`wg genkey` → `ripws genkey`).

```sh
ripws genkey
# Example output: 8x/P9mZ3qF1bR6wN2vT4xK7jL0sH5aBcDeFgHiJk=
```

---

## genuuid

Generates a fresh UUIDv4 — the client identity used as the WebSocket URL path
and the `UUID` field in `[Peer]` sections.

```sh
ripws genuuid
# Example output: f47ac10b-58cc-4372-a567-0e02b2c3d479
```

---

## Usage Examples

### 1. Kernel Mode (Basic Tunnel)

The most basic configuration. Both server and client use TUN devices. Requires
`CAP_NET_ADMIN` and `/dev/net/tun`.

```sh
# Generate credentials
UUID="$(ripws genuuid)"
PSK="$(ripws genkey)"

# --- Server config (/etc/ripws/ws0.conf) ---
cat > /etc/ripws/ws0.conf << EOF
[Interface]
Address    = 10.7.0.1/24
ListenPort = 8080
PrivateKey = $PSK
MTU        = 1420

[Peer]
UUID  = $UUID
AllowedIPs = 10.7.0.2/32
EOF

# --- Client config ---
cat > /etc/ripws/ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = ws://SERVER:8080
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 30
EOF

# Bring up
sudo ripws up ws0.conf        # on both server and client

# Verify connectivity
ping -c 4 10.7.0.1   # client → server
ping -c 4 10.7.0.2   # server → client

# Bring down
sudo ripws down ws0.conf
```

---

### 2. Userspace Mode (No Privileges)

Uses gVisor netstack. No root, no TUN device required.

```sh
# --- Server config (kernel mode, with TUN) ---
cat > /etc/ripws/ws0.conf << EOF
[Interface]
Address    = 10.8.0.1/24
ListenPort = 8080
PrivateKey = $PSK
Mode       = kernel
MTU        = 1420

[Peer]
UUID  = $UUID
AllowedIPs = 10.8.0.2/32
EOF
sudo ripws up ws0.conf

# --- Client config (userspace, no privileges) ---
cat > ws0.conf << EOF
[Interface]
Address    = 10.8.0.2/24
PrivateKey = $PSK
Mode       = userspace
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = ws://SERVER:8080
AllowedIPs = 10.8.0.0/24
PersistentKeepalive = 30
EOF
ripws up ws0.conf          # no sudo needed
```

> In userspace mode, `AllowedIPs` controls which CIDRs are routed through the
> gVisor netstack. Use `0.0.0.0/0` for full-tunnel or a specific subnet like
> `10.8.0.0/24`.

---

### 3. Userspace NAT Gateway

Runs the client as a NAT gateway, forwarding transit traffic from the server to
the internet via the client. No privileges required on the client side.

```
Server (kernel TUN 10.7.0.1/24)
    │  ip route add 1.1.1.1/32 8.8.8.8/32 via 10.7.0.2
    │  WebSocket ws://server:8080
Client (userspace netstack 10.7.0.2, NAT = true)
    │  gVisor TCP/UDP Forwarder → net.Dial → Internet
Internet (1.1.1.1, 8.8.8.8, ...)
```

```sh
# --- Client config (NAT gateway, no privileges) ---
cat > ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
Mode       = userspace
NAT        = true
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = ws://SERVER:8080
AllowedIPs = 10.7.0.0/24
PersistentKeepalive = 30
EOF
ripws up ws0.conf

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

### 4. Cloudflare Tunnel (wss://)

Uses Cloudflare Tunnel (`cloudflared`) as a reverse proxy. TLS encryption + CDN
traversal via `wss://`.

```sh
# --- Server (Docker container, kernel TUN 10.7.0.1/24) ---
# Cloudflare Tunnel reverse-proxies localhost:8080
sudo ripws up ws0.conf
# Bind iperf3 to the tunnel IP
iperf3 -s -p 5201 -B 10.7.0.1

# --- cloudflared (running on the host) ---
cloudflared tunnel --no-autoupdate --protocol http2 run --token <CF_TOKEN>
# --protocol http2 is required (QUIC datagram handler fails in some environments)

# --- Client (on host, userspace mode) ---
cat > ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
Mode       = userspace
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = wss://ripws-test.example.com
AllowedIPs = 10.7.0.0/24
PersistentKeepalive = 30
EOF
ripws up ws0.conf

# iperf3 (through Cloudflare, if port-forward is set up)
iperf3 -c 10.7.0.1 -p 5201 -t 15 -P 8       # TCP 8 parallel
iperf3 -c 10.7.0.1 -p 5201 -t 15 -u -b 500M  # UDP 500Mbps
```

Kernel mode through Cloudflare also works — just use `Mode = kernel` in the
client config and assign the TUN address manually after `ripws up`.

> **Silent Drop + Cloudflare:** Probes with bad UUID/PSK are RST'd at the
> origin, but Cloudflare translates this to HTTP 502 for the client. The origin
> itself returns no HTTP response.

---

### 5. Multiple Clients (JSON Config File)

The server's `PrivateKey` (PSK) is the default for all peers. Per-peer
`PresharedKey` overrides allow different PSKs per client.

```sh
# Generate multiple UUID-PSK pairs
UUID_A="$(ripws genuuid)"; PSK_A="$(ripws genkey)"
UUID_B="$(ripws genuuid)"; PSK_B="$(ripws genkey)"

# Server config with multiple peers
cat > /etc/ripws/ws0.conf << EOF
[Interface]
Address    = 10.7.0.1/24
ListenPort = 8080
PrivateKey = $DEFAULT_PSK
MTU        = 1420

[Peer]
UUID  = $UUID_A
PresharedKey = $PSK_A
AllowedIPs = 10.7.0.2/32

[Peer]
UUID  = $UUID_B
PresharedKey = $PSK_B
AllowedIPs = 10.7.0.3/32
EOF

sudo ripws up ws0.conf

# Each client connects with its own UUID/PSK
# Client A: Endpoint = ws://SERVER:8080, UUID = $UUID_A, PrivateKey = $PSK_A
# Client B: Endpoint = ws://SERVER:8080, UUID = $UUID_B, PrivateKey = $PSK_B
```

---

## Test Scripts

| Script | Description |
|--------|-------------|
| `scripts/test.sh` | Kernel mode L3 ping + Silent Drop (Docker, 2 containers) |
| `scripts/test-nat.sh` | NAT gateway test (server → tunnel → client NAT → internet, TCP/UDP/DNS) |
| `scripts/load-test.sh` | iperf3 throughput benchmark (kernel mode, Docker) |
| `scripts/reconnect-test.sh` | Reconnection + KeepAlive robustness (server restart, hard kill, idle) |
| `scripts/multiport-test.sh` | Multi-port concurrent connection test (32 iperf3 streams) |
| `scripts/cloudflare-test.sh` | Cloudflare kernel mode E2E (ping + Silent Drop) |
| `scripts/cloudflare-load-test.sh` | Cloudflare kernel mode load test (iperf3 + 1000 pings) |

---

## Docker Tests

### Kernel Mode (L3 ping, Silent Drop)

```
ripws-client (ws0 10.7.0.2/24)  --ws-->  ripws-server (ws0 10.7.0.1/24)
```

Both containers run with `--cap-add=NET_ADMIN --device=/dev/net/tun`. The
entrypoint (`scripts/entrypoint.sh`) generates an INI config from environment
variables and runs `ripws up`.

```sh
bash scripts/test.sh
```

Expected: `ping -c 4 10.7.0.1` (client→server) and `ping -c 4 10.7.0.2`
(server→client) both succeed; a probe with bad UUID/PSK yields
`Connection reset by peer` with **no HTTP status line** (Silent Drop).

### NAT Gateway

```
ripws-server (ws0 10.7.0.1/24) → tunnel → ripws-client (userspace 10.7.0.2, NAT=true) → internet
```

```sh
bash scripts/test-nat.sh
```

### Manual Docker Commands

```sh
nix build .#ripws
mkdir -p bin && cp -L result/bin/ripws bin/ripws
sudo docker compose build

# Generate credentials
UUID="$(nix run .#ripws -- genuuid)"
PSK="$(nix run .#ripws -- genkey)"
mkdir -p config && printf 'UUID=%s\nPSK=%s\n' "$UUID" "$PSK" > config/creds.env

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
main.go                      up/down/genkey/genuuid dispatch
flake.nix                    buildGoModule (static binary)
internal/
  config/                    credentials, PSK validation (SPEC 2.2)
    quickconf.go             wg-quick-compatible INI parser (Mode, NAT fields)
  proto/                     IP-version nibble, keep-alive, exponential backoff
  tunnel/
    tunnel.go                Interface abstract contract
    tun_linux.go             kernel TUN backend (songgao/water)
    netstack.go              gVisor userspace backend (netstack)
    setup_linux.go           wg-quick-style ifup/ifdown (addr/MTU/routes/DNS/hooks)
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
| L3 connectivity (kernel mode) | PASS |
| Silent Drop security | PASS |
| Userspace NAT gateway | PASS (TCP/UDP/DNS, tunnel → internet) |
| iperf3 throughput (kernel) | PASS (1.77 Gbps -P64) |
| Multi-port concurrency | PASS (32/32) |
| Reconnection & KeepAlive | PASS |
| Race detector & unit tests | PASS |
| Cloudflare tunnel (kernel) | PASS (wss://, bidirectional, RTT ~232ms) |
