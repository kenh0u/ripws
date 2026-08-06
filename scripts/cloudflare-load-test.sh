#!/usr/bin/env bash
# Cloudflare tunnel heavy load test: iperf3 (TCP/UDP/multi-stream) + ping RTT.
#
# Client runs on the host via `ripws up` (kernel TUN mode).
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
UUID="$(nix run .#ripws -- genuuid)"
PSK="$(nix run .#ripws -- genkey)"
mkdir -p config && printf 'UUID=%s\nPSK=%s\n' "$UUID" "$PSK" > config/creds.env
export RIPWS_UUID="$UUID" RIPWS_PSK="$PSK"
DC="sudo -E docker compose -f docker-compose.yml -f docker-compose.cf.yml"
$DC down --remove-orphans 2>/dev/null || true
$DC up -d server
sleep 3

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

# --- host client via `ripws up` ---
say "Starting ripws client on host (kernel TUN)"
CLIENT_CONF="$(mktemp /tmp/ripws-client-XXXX.conf)"
cat > "$CLIENT_CONF" <<EOF
[Interface]
Address = 10.7.0.2/24
PrivateKey = $PSK
Mode = kernel
[Peer]
PublicKey = $UUID
Endpoint = wss://ripws-test.kenhou.xyz/$UUID
AllowedIPs = 10.7.0.0/24
PersistentKeepalive = 30
EOF

CLOG="$(mktemp)"
sudo "$REPO/result/bin/ripws" up "$CLIENT_CONF" > "$CLOG" 2>&1 &
CLPID=$!
sleep 3
grep "connected" "$CLOG" || { echo "client not connected"; tail -20 "$CLOG"; exit 1; }
echo "client connected, dev=ws0"

# --- Ping RTT (1000 pings) ---
say "Ping RTT test: 1000 pings for median"
sudo ping -c 1000 -i 0.01 -W 5 -I ws0 10.7.0.1 2>&1 | tee /tmp/cf-ping.txt | tail -5
echo "--- RTT stats ---"
tail -1 /tmp/cf-ping.txt

# --- iperf3 server on the server container ---
sudo docker exec -d ripws-server iperf3 -s -p 5201
sleep 1

# --- iperf3 TCP (client -> server, 15s) ---
say "iperf3 TCP client->server through Cloudflare (15s)"
$IPERF3 -c 10.7.0.1 -p 5201 -t 15 -B 10.7.0.2 2>&1 | tee /tmp/cf-tcp-c2s.txt | tail -8

# --- iperf3 TCP reverse (server -> client, 15s) ---
say "iperf3 TCP server->client (reverse, 15s)"
$IPERF3 -c 10.7.0.1 -p 5201 -t 15 -R -B 10.7.0.2 2>&1 | tee /tmp/cf-tcp-s2c.txt | tail -8

# --- iperf3 TCP -P 8 (8 parallel, 15s) ---
say "iperf3 TCP -P 8 parallel (15s)"
$IPERF3 -c 10.7.0.1 -p 5201 -t 15 -P 8 -B 10.7.0.2 2>&1 | tee /tmp/cf-tcp-p8.txt | tail -12

# --- iperf3 TCP -P 32 (32 parallel, 15s) ---
say "iperf3 TCP -P 32 parallel (15s)"
$IPERF3 -c 10.7.0.1 -p 5201 -t 15 -P 32 -B 10.7.0.2 2>&1 | tee /tmp/cf-tcp-p32.txt | tail -12

# --- iperf3 UDP (client -> server, 15s) ---
say "iperf3 UDP client->server through Cloudflare (15s)"
$IPERF3 -c 10.7.0.1 -p 5201 -t 15 -u -b 100M -B 10.7.0.2 2>&1 | tee /tmp/cf-udp.txt | tail -8

# --- iperf3 UDP high bitrate ---
say "iperf3 UDP 500M (15s)"
$IPERF3 -c 10.7.0.1 -p 5201 -t 15 -u -b 500M -B 10.7.0.2 2>&1 | tee /tmp/cf-udp-500.txt | tail -8

# --- Cleanup ---
say "Cleanup"
sudo "$REPO/result/bin/ripws" down "$CLIENT_CONF" 2>/dev/null || sudo kill "$CLPID" 2>/dev/null || true
sleep 1
kill "$CFPID" 2>/dev/null || true
sudo docker exec ripws-server pkill iperf3 2>/dev/null || true
$DC down 2>&1 | tail -2
rm -f "$CFLOG" "$CLOG" "$CLIENT_CONF"
echo "Done."
