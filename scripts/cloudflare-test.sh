#!/usr/bin/env bash
# Cloudflare-tunnel E2E test for RIPWS.
#
# Topology:
#   host TUN 10.7.0.2/24
#     └─ ripws client (host, sudo) ──wss://ripws-test.kenhou.xyz/<uuid>──▶
#           Cloudflare edge ──▶ cloudflared (host) ──▶ localhost:18080
#           ──▶ ripws server container (TUN 10.7.0.1/24)
#
# Prereqs:
#   - CF_TUNNEL_TOKEN env var set (the cloudflared tunnel token).
#   - The server container exposes :8080 on host 127.0.0.1:18080
#     (docker-compose.cf.yml override).
#   - sudo is nopasswd (client needs CAP_NET_ADMIN + /dev/net/tun on the host).
#
# Usage: CF_TUNNEL_TOKEN=eyJ... bash scripts/cloudflare-test.sh

set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$PWD"

if [ -z "${CF_TUNNEL_TOKEN:-}" ]; then
    echo "error: CF_TUNNEL_TOKEN is not set" >&2
    exit 2
fi

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
DC="sudo -E docker compose -f docker-compose.yml -f docker-compose.cf.yml"

say "Building & starting server (port 18080 exposed to host)"
nix build .#ripws --no-link
mkdir -p bin && rm -f bin/ripws && cp -L result/bin/ripws bin/ripws
$DC build

CRED_JSON="$(nix run .#ripws -- gen-config)"
UUID="$(printf '%s' "$CRED_JSON" | jq -r 'keys[0]')"
PSK="$(printf '%s' "$CRED_JSON" | jq -r ".[\"$UUID\"]")"
mkdir -p config && printf '%s' "$CRED_JSON" > config/server.json
echo "UUID=$UUID  PSK=$PSK"

export RIPWS_UUID="$UUID" RIPWS_PSK="$PSK"
$DC down --remove-orphans 2>/dev/null || true
# Only the server uses the container path; the client runs on the host.
$DC up -d server
sleep 3
sudo docker exec ripws-server ip addr show tun0 2>&1 | grep inet || true

say "Starting cloudflared tunnel (host)"
# cloudflared forwards https://ripws-test.kenhou.xyz -> http://localhost:18080
CLOUDFLARED_LOG="$(mktemp)"
nix-shell -p cloudflared --run \
    "cloudflared tunnel --no-autoupdate --protocol http2 run --token '$CF_TUNNEL_TOKEN'" \
    > "$CLOUDFLARED_LOG" 2>&1 &
CF_PID=$!
echo "cloudflared pid=$CF_PID (log: $CLOUDFLARED_LOG)"

# Wait for cloudflared to register the connection.
say "Waiting for cloudflared to be ready"
for _ in $(seq 1 30); do
    if grep -q "Registered tunnel connection" "$CLOUDFLARED_LOG" 2>/dev/null \
       || grep -qi "connection.*registered\|conn.*ready" "$CLOUDFLARED_LOG" 2>/dev/null; then
        echo "cloudflared ready"; break
    fi
    sleep 1
done
tail -3 "$CLOUDFLARED_LOG" || true

# Sanity: the public hostname should respond (silently RST) to a bad probe.
say "Probing public hostname (expect silent reset, no HTTP status)"
curl -sS -i --max-time 10 \
    "https://ripws-test.kenhou.xyz/00000000-0000-0000-0000-000000000000" \
    -H "Authorization: Bearer bad" 2>&1 | head -4 || true

# --- host client ---------------------------------------------------------
say "Detecting existing host tun devices"
BEFORE="$(ls /sys/class/net/ 2>/dev/null | grep -E '^tun' | sort || true)"
echo "before: ${BEFORE:-<none>}"

say "Starting ripws client on the host (sudo, kernel TUN)"
CLIENT_LOG="$(mktemp)"
sudo "$REPO/result/bin/ripws" client \
    --url "wss://ripws-test.kenhou.xyz/$UUID" \
    --psk "$PSK" \
    --mode kernel \
    --ping-interval 30s \
    > "$CLIENT_LOG" 2>&1 &
CLIENT_PID=$!
echo "client pid=$CLIENT_PID (log: $CLIENT_LOG)"

# Discover the new tun device.
DEV=""
for _ in $(seq 1 50); do
    AFTER="$(ls /sys/class/net/ 2>/dev/null | grep -E '^tun' | sort || true)"
    DEV="$(comm -13 <(printf '%s\n' "$BEFORE") <(printf '%s\n' "$AFTER") | head -1)"
    [ -n "$DEV" ] && break
    sleep 0.2
done
if [ -z "$DEV" ]; then
    echo "error: no new tun device appeared" >&2
    tail -20 "$CLIENT_LOG"; kill $CLIENT_PID 2>/dev/null || true; kill $CF_PID 2>/dev/null || true
    exit 1
fi
echo "host TUN device: $DEV"
sudo ip addr add 10.7.0.2/24 dev "$DEV"
sudo ip link set up dev "$DEV"

sleep 3
say "Client log so far"
tail -8 "$CLIENT_LOG" || true

say "ping 10.7.0.1 through Cloudflare tunnel (host -> edge -> cloudflared -> server TUN)"
sudo ping -c 5 -W 3 -I "$DEV" 10.7.0.1

say "ping 10.7.0.2 from server container (reverse direction)"
sudo docker exec ripws-server ping -c 3 -W 3 10.7.0.2 || true

say "iperf3 through Cloudflare (TCP, 5s)"
sudo docker exec ripws-server sh -c 'pkill iperf3 2>/dev/null; iperf3 -s -p 5201 -1' &
sleep 1
sudo iperf3 -c 10.7.0.1 -p 5201 -t 5 -I "$DEV" 2>&1 | tail -12 || true

say "Cleanup"
sudo kill "$CLIENT_PID" 2>/dev/null || true
sleep 1
sudo ip link del "$DEV" 2>/dev/null || true
kill "$CF_PID" 2>/dev/null || true
$DC down 2>&1 | tail -2
rm -f "$CLOUDFLARED_LOG" "$CLIENT_LOG"
echo "Done."
