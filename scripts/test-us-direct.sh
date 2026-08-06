#!/usr/bin/env bash
# End-to-end RIPWS test: client userspace (gVisor netstack) + server kernel.
#
# Topology:
#
#   ripws-client (userspace netstack 10.8.0.2)
#       |  WebSocket ws://server:8080/<uuid>  (Bearer PSK)
#       |  TCP/UDP forward: 0.0.0.0:5201 -> 10.8.0.1:5201 (via netstack)
#   ripws-server (tun0 10.8.0.1/24, iperf3 -s -B 10.8.0.1)
#
# Tests:
#   1. ICMP ping (via --us-ping, 10 probes)
#   2. iperf3 TCP c2s, s2c (reverse), multi-stream
#   3. iperf3 UDP
#
# All iperf3 runs from inside the client container, connecting to 127.0.0.1:5201,
# which is forwarded through the gVisor netstack to the server's TUN address.

DC="sudo -E docker compose -f docker-compose.us.yml"
IPERF3="/usr/bin/iperf3"

set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

# 1. Build.
say "Building ripws with Nix"
nix build .#ripws --no-link
mkdir -p bin
rm -f bin/ripws
cp -L result/bin/ripws bin/ripws

# 2. Credentials.
say "Generating credentials"
CRED_JSON="$(nix run .#ripws -- gen-config)"
UUID="$(printf '%s' "$CRED_JSON" | jq -r 'keys[0]')"
PSK="$(printf '%s' "$CRED_JSON" | jq -r ".[\"$UUID\"]")"
mkdir -p config
printf '%s\n' "$CRED_JSON" > config/server.json
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
sudo docker logs ripws-server 2>&1 | tail -10 || true
echo "--- client logs ---"
sudo docker logs ripws-client 2>&1 | tail -15 || true

# 5. ICMP ping (from --us-ping in client logs).
say "ICMP ping results (from client logs)"
sleep 12  # wait for 10 pings at 1s interval
sudo docker logs ripws-client 2>&1 | grep -E "ping (seq=|.*loss)" || true

# 6. iperf3 TCP c2s.
say "iperf3 TCP client->server (via userspace forward)"
sudo docker exec ripws-client $IPERF3 -c 127.0.0.1 -p 5201 -t 10 -J | jq '{ throughput_Mbps: (.end.sum_received.bits_per_second / 1e6 | floor), retransmits: .end.sum_sent.retransmits }'

# 7. iperf3 TCP s2c (reverse).
say "iperf3 TCP server->client (reverse)"
sudo docker exec ripws-client $IPERF3 -c 127.0.0.1 -p 5201 -t 10 -R -J | jq '{ throughput_Mbps: (.end.sum_received.bits_per_second / 1e6 | floor) }'

# 8. iperf3 TCP multi-stream.
say "iperf3 TCP -P 8 (8 parallel streams)"
sudo docker exec ripws-client $IPERF3 -c 127.0.0.1 -p 5201 -t 10 -P 8 -J | jq '{ throughput_Mbps: (.end.sum_received.bits_per_second / 1e6 | floor) }'

# 9. iperf3 UDP.
say "iperf3 UDP 200Mbps"
sudo docker exec ripws-client $IPERF3 -c 127.0.0.1 -p 5201 -t 10 -u -b 200M -J 2>&1 | jq '{ throughput_Mbps: ((.end.sum.bits_per_second // 0) / 1e6 | floor), jitter_ms: ((.end.sum.jitter_ms // 0) | . * 1000 | floor / 1000), lost_pct: (.end.sum.lost_percent // 0) }'

# 10. iperf3 UDP high bandwidth.
say "iperf3 UDP 1Gbps"
sudo docker exec ripws-client $IPERF3 -c 127.0.0.1 -p 5201 -t 10 -u -b 1G -J 2>&1 | jq '{ throughput_Mbps: ((.end.sum.bits_per_second // 0) / 1e6 | floor), jitter_ms: ((.end.sum.jitter_ms // 0) | . * 1000 | floor / 1000), lost_pct: (.end.sum.lost_percent // 0) }'

# 11. Cleanup.
say "Done — leaving containers running for inspection"
echo "  sudo docker logs ripws-client"
echo "  ${DC} down"
