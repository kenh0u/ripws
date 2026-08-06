#!/usr/bin/env bash
# Reconnection & KeepAlive robustness test for RIPWS.
#
# Tests:
#   1. Client reconnects after server restart (with backoff).
#   2. Client reconnects after network interruption (kill server, restart).
#   3. KeepAlive: client pings keep idle connection alive.
#
# Uses docker compose with kernel TUN mode.

set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$PWD"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

# Build
say "Building ripws"
nix build .#ripws --no-link
mkdir -p bin && rm -f bin/ripws && cp -L result/bin/ripws bin/ripws

# Generate credentials
UUID="$(nix run .#ripws -- genuuid)"
PSK="$(nix run .#ripws -- genkey)"
mkdir -p config && printf 'UUID=%s\nPSK=%s\n' "$UUID" "$PSK" > config/creds.env
echo "UUID=$UUID  PSK=$PSK"

export RIPWS_UUID="$UUID" RIPWS_PSK="$PSK"
DC="sudo -E docker compose -f docker-compose.yml"

say "Starting fresh server"
$DC down --remove-orphans 2>/dev/null || true
$DC up -d server
sleep 3
sudo docker exec ripws-server ip addr show tun0 2>&1 | grep "inet " || true

# --- Test 1: Client reconnect after server restart ---
say "TEST 1: Start client, verify connectivity, restart server, verify reconnect"

CLIENT_LOG="$(mktemp)"
$DC up -d client
sleep 3
CLIENT_DEV=$(sudo docker exec ripws-client ip -o link show tun0 2>/dev/null | awk -F: '{print $2}' | tr -d ' ' || true)
echo "client tun: ${CLIENT_DEV:-none}"

say "ping server from client (before restart)"
sudo docker exec ripws-client ping -c 3 -W 2 10.7.0.1

say "Restarting server (simulates server crash)"
$DC restart server
echo "waiting for server to come back..."
sleep 5
sudo docker exec ripws-server ip addr show tun0 2>&1 | grep "inet " || true

say "Waiting for client to reconnect (backoff with jitter, up to 30s)"
SUCCESS=0
for i in $(seq 1 15); do
    sleep 2
    if sudo docker exec ripws-client ping -c 1 -W 2 10.7.0.1 2>/dev/null; then
        echo "client reconnected after ~$((i*2))s"
        SUCCESS=1
        break
    fi
    echo "attempt $i: not yet..."
done

if [ "$SUCCESS" -eq 0 ]; then
    echo "FAIL: client did not reconnect within 30s"
    sudo docker logs ripws-client --tail 20 2>&1
    exit 1
fi

say "ping server from client (after reconnect)"
sudo docker exec ripws-client ping -c 3 -W 2 10.7.0.1

# --- Test 2: KeepAlive — idle connection stays alive ---
say "TEST 2: KeepAlive — idle 60s, then verify still connected"
echo "sleeping 60s (client ping-interval=30s, so ~2 pings should fire)..."
sleep 60

say "ping after 60s idle"
sudo docker exec ripws-client ping -c 3 -W 2 10.7.0.1

# --- Test 3: Server kill (not restart) and reconnect ---
say "TEST 3: Kill server container hard (docker kill), verify reconnect"
sudo docker kill ripws-server 2>/dev/null || true
echo "server killed, waiting 3s..."
sleep 3
$DC up -d server
sleep 5

say "Waiting for client to reconnect after hard kill"
SUCCESS=0
for i in $(seq 1 15); do
    sleep 2
    if sudo docker exec ripws-client ping -c 1 -W 2 10.7.0.1 2>/dev/null; then
        echo "client reconnected after ~$((i*2))s"
        SUCCESS=1
        break
    fi
    echo "attempt $i: not yet..."
done

if [ "$SUCCESS" -eq 0 ]; then
    echo "FAIL: client did not reconnect after hard kill"
    sudo docker logs ripws-client --tail 20 2>&1
    exit 1
fi

say "All reconnection tests PASSED"

# Show client logs for verification
say "Client log tail"
sudo docker logs ripws-client --tail 15 2>&1 || true

say "Cleanup"
$DC down 2>&1 | tail -2
rm -f "$CLIENT_LOG"
echo "Done."
