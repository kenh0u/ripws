#!/usr/bin/env bash
# NAT test: server(kernel) -> tunnel -> client(userspace NAT) -> internet
#
# Topology:
#   ripws-server (ws0 10.7.0.1/24)
#       |  route 1.1.1.1/32, 8.8.8.8/32 via tunnel (AllowedIPs)
#       |  WebSocket ws://server:8080/<uuid>
#   ripws-client (userspace netstack 10.7.0.2, NAT enabled)
#       |  net.Dial to external IPs via Docker bridge (internet access)
#   internet (1.1.1.1, 8.8.8.8)
#
# Tests:
#   1. TCP: curl http://1.1.1.1 from server (through tunnel → client NAT → internet)
#   2. TCP: curl https://1.1.1.1 (TLS)
#   3. UDP: dig @8.8.8.8 example.com from server

DC="sudo -E docker compose -f docker-compose.nat.yml"

set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

# 1. Build.
say "Building ripws with Nix"
nix build .#ripws --no-link
mkdir -p bin && rm -f bin/ripws && cp -L result/bin/ripws bin/ripws

# 2. Credentials.
say "Generating credentials"
UUID="$(nix run .#ripws -- genuuid)"
PSK="$(nix run .#ripws -- genkey)"
mkdir -p config
printf 'UUID=%s\nPSK=%s\n' "$UUID" "$PSK" > config/creds.env
echo "UUID=$UUID  PSK=${PSK:0:10}..."

# 3. Build image and launch.
say "Building Docker image"
${DC} build

say "Starting containers"
export RIPWS_UUID="$UUID"
export RIPWS_PSK="$PSK"
${DC} down --remove-orphans 2>/dev/null || true
${DC} up -d server
sleep 3
${DC} up -d client

# 4. Wait for tunnel.
say "Waiting for tunnel to establish"
sleep 5

echo "--- server logs ---"
sudo docker logs ripws-server 2>&1 | tail -15 || true
echo "--- client logs ---"
sudo docker logs ripws-client 2>&1 | tail -15 || true

# Verify tunnel is up
if ! sudo docker logs ripws-client 2>&1 | grep -q "connected"; then
    say "ERROR: client not connected"
    sudo docker logs ripws-client 2>&1 | tail -30
    exit 1
fi

# 5. TCP test: curl http://1.1.1.1 from server through tunnel → client NAT → internet
say "TCP test: curl http://1.1.1.1 from server (via tunnel → NAT → internet)"
sudo docker exec ripws-server curl -sS --max-time 10 -o /dev/null -w "HTTP %{http_code}, %{time_total}s, %{size_download} bytes\n" http://1.1.1.1/ 2>&1 || echo "FAILED"

# 6. TCP test: curl https://1.1.1.1 (TLS)
say "TCP test: curl https://1.1.1.1 (TLS) from server"
sudo docker exec ripws-server curl -sS --max-time 10 -o /dev/null -w "HTTP %{http_code}, %{time_total}s\n" https://1.1.1.1/ 2>&1 || echo "FAILED"

# 7. UDP test: DNS query to 8.8.8.8
say "UDP test: dig @8.8.8.8 example.com from server"
sudo docker exec ripws-server sh -c "dig @8.8.8.8 example.com +short +time=5 2>&1" || echo "dig not available"

# 8. Show NAT logs
say "NAT activity logs (client)"
sudo docker logs ripws-client 2>&1 | grep "nat:" | tail -20 || true

# 9. Cleanup
say "Done — leaving containers running for inspection"
echo "  sudo docker logs ripws-client"
echo "  ${DC} down"
