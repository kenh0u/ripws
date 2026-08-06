#!/usr/bin/env bash
# Cloudflare tunnel test: client userspace (gVisor) + server kernel via CF.
#
# Server: Docker container, kernel TUN 10.7.0.1/24, iperf3 -B 10.7.0.1
# Client: host process, userspace netstack 10.7.0.2, port-forward 127.0.0.1:5201 -> 10.7.0.1:5201
# Path:   host client (netstack) -> wss://ripws-test.kenhou.xyz -> CF tunnel -> server:8080
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$PWD"

CF=/nix/store/mr8zwwhs19fh77m4fnbd6jhlhpxwkkb1-cloudflared-2026.5.2/bin/cloudflared
IPERF3=/nix/store/nn14za4zh6ahwpqvbhwbqh6smvhbjlgk-iperf-3.20/bin/iperf3
TOKEN="${CF_TUNNEL_TOKEN:?need CF_TUNNEL_TOKEN}"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

# --- Build & start server ---
say "Building & starting server"
nix build .#ripws --no-link
mkdir -p bin && rm -f bin/ripws && cp -L result/bin/ripws bin/ripws
CRED_JSON="$(nix run .#ripws -- gen-config)"
UUID="$(printf '%s' "$CRED_JSON" | jq -r 'keys[0]')"
PSK="$(printf '%s' "$CRED_JSON" | jq -r ".[\"$UUID\"]")"
mkdir -p config && printf '%s' "$CRED_JSON" > config/server.json
export RIPWS_UUID="$UUID" RIPWS_PSK="$PSK"
DC="sudo -E docker compose -f docker-compose.yml -f docker-compose.cf.yml"
$DC down --remove-orphans 2>/dev/null || true
$DC up -d server
sleep 3
sudo docker exec -d ripws-server iperf3 -s -p 5201 -B 10.7.0.1
sleep 1

# --- cloudflared ---
say "Starting cloudflared"
CFLOG="$(mktemp)"
$CF tunnel --no-autoupdate --protocol http2 run --token "$TOKEN" > "$CFLOG" 2>&1 &
CFPID=$!
for _ in $(seq 1 20); do
    grep -q "Registered tunnel connection" "$CFLOG" 2>/dev/null && break
    sleep 1
done
grep "Registered" "$CFLOG" | head -1 || { echo "cloudflared failed"; tail -10 "$CFLOG"; exit 1; }

# --- host client (userspace mode) ---
say "Starting ripws client on host (userspace mode)"
CLOG="$(mktemp)"
sudo "$REPO/result/bin/ripws" client \
    --url "wss://ripws-test.kenhou.xyz/$UUID" \
    --psk "$PSK" --mode userspace \
    --us-addr 10.7.0.2 --us-route 10.7.0.0/24 \
    --us-ping 10.7.0.1 --us-ping-count 100 --us-ping-interval 0.1s \
    --us-forward 127.0.0.1:5201=10.7.0.1:5201 \
    --us-udp-forward 127.0.0.1:5201=10.7.0.1:5201 \
    --ping-interval 30s > "$CLOG" 2>&1 &
CLPID=$!
sleep 5
grep "connected" "$CLOG" || { echo "client not connected"; tail -20 "$CLOG"; exit 1; }
echo "client connected (userspace)"

# --- Ping (via --us-ping, 100 probes at 0.1s interval = ~10s) ---
say "ICMP ping (100 probes via userspace netstack)"
sleep 12
grep -E "ping (seq=|.*loss)" "$CLOG" | tail -5

# --- iperf3 TCP c2s ---
say "iperf3 TCP client->server through Cloudflare (15s)"
$IPERF3 -c 127.0.0.1 -p 5201 -t 15 -J 2>&1 | jq '{ throughput_Mbps: (.end.sum_received.bits_per_second / 1e6 | floor), retransmits: .end.sum_sent.retransmits }'

# --- iperf3 TCP reverse ---
say "iperf3 TCP server->client (reverse, 15s)"
$IPERF3 -c 127.0.0.1 -p 5201 -t 15 -R -J 2>&1 | jq '{ throughput_Mbps: (.end.sum_received.bits_per_second / 1e6 | floor) }'

# --- iperf3 TCP -P 8 ---
say "iperf3 TCP -P 8 parallel (15s)"
$IPERF3 -c 127.0.0.1 -p 5201 -t 15 -P 8 -J 2>&1 | jq '{ throughput_Mbps: (.end.sum_received.bits_per_second / 1e6 | floor) }'

# --- iperf3 TCP -P 32 ---
say "iperf3 TCP -P 32 parallel (15s)"
$IPERF3 -c 127.0.0.1 -p 5201 -t 15 -P 32 -J 2>&1 | jq '{ throughput_Mbps: (.end.sum_received.bits_per_second / 1e6 | floor) }'

# --- iperf3 UDP 100M ---
say "iperf3 UDP 100M (15s)"
$IPERF3 -c 127.0.0.1 -p 5201 -t 15 -u -b 100M -J 2>&1 | jq '{ throughput_Mbps: ((.end.sum.bits_per_second // 0) / 1e6 | floor), jitter_ms: ((.end.sum.jitter_ms // 0) | . * 1000 | floor / 1000), lost_pct: (.end.sum.lost_percent // 0) }'

# --- iperf3 UDP 500M ---
say "iperf3 UDP 500M (15s)"
$IPERF3 -c 127.0.0.1 -p 5201 -t 15 -u -b 500M -J 2>&1 | jq '{ throughput_Mbps: ((.end.sum.bits_per_second // 0) / 1e6 | floor), jitter_ms: ((.end.sum.jitter_ms // 0) | . * 1000 | floor / 1000), lost_pct: (.end.sum.lost_percent // 0) }'

# --- Cleanup ---
say "Cleanup"
sudo kill "$CLPID" 2>/dev/null || true
sleep 1
kill "$CFPID" 2>/dev/null || true
sudo docker exec ripws-server pkill iperf3 2>/dev/null || true
$DC down 2>&1 | tail -2
rm -f "$CFLOG" "$CLOG"
echo "Done."
