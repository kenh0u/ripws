#!/usr/bin/env bash
# RIPWS load test suite (kernel TUN mode).
#
# Brings up the server + client containers, then runs:
#   1. iperf3 TCP throughput (client -> server, over the tunnel)
#   2. iperf3 TCP throughput (server -> client, reverse, --reverse)
#   3. iperf3 UDP throughput + packet loss / jitter
#   4. iperf3 with many parallel streams (-P 16)
#   5. Many concurrent TCP connections across many ports (a small Go helper
#      is overkill; we use iperf3 -P plus parallel nc-style probes via iperf3)
#
# Results are printed and appended to scripts/load-results.txt.

set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$PWD"
RESULTS="$REPO/scripts/load-results.txt"
: > "$RESULTS"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
record() { printf '### %s\n' "$1" | tee -a "$RESULTS"; }
run() { "$@" 2>&1 | tee -a "$RESULTS"; }

DC="sudo -E docker compose"

# --- start stack ---------------------------------------------------------
say "Building & starting kernel-mode stack"
nix build .#ripws --no-link
mkdir -p bin && rm -f bin/ripws && cp -L result/bin/ripws bin/ripws
$DC build

UUID="$(nix run .#ripws -- genuuid)"
PSK="$(nix run .#ripws -- genkey)"
mkdir -p config && printf 'UUID=%s\nPSK=%s\n' "$UUID" "$PSK" > config/creds.env
echo "UUID=$UUID PSK=$PSK" | tee -a "$RESULTS"

export RIPWS_UUID="$UUID" RIPWS_PSK="$PSK"
$DC down --remove-orphans 2>/dev/null || true
$DC up -d server
sleep 2
$DC up -d client
sleep 3

# sanity
say "Sanity ping"
sudo docker exec ripws-client ping -c 2 -W 2 10.7.0.1

# --- iperf3 server on the server container's TUN IP ----------------------
say "Starting iperf3 server (10.7.0.1:5201)"
# single persistent server; reused across all tests
sudo docker exec -d ripws-server iperf3 -s -p 5201
sleep 1

say "iperf3 TCP  client->server (over tunnel)"
record "iperf3 TCP client->server"
run sudo docker exec ripws-client iperf3 -c 10.7.0.1 -p 5201 -t 10

say "iperf3 TCP  server->client (--reverse)"
record "iperf3 TCP server->client (reverse)"
run sudo docker exec ripws-client iperf3 -c 10.7.0.1 -p 5201 -t 10 --reverse

say "iperf3 UDP  client->server"
record "iperf3 UDP client->server"
run sudo docker exec ripws-client iperf3 -c 10.7.0.1 -p 5201 -t 10 -u -b 200M

say "iperf3 TCP 16 parallel streams"
record "iperf3 TCP -P 16"
run sudo docker exec ripws-client iperf3 -c 10.7.0.1 -p 5201 -t 10 -P 16

say "iperf3 TCP 64 parallel streams (connection pressure)"
record "iperf3 TCP -P 64"
run sudo docker exec ripws-client iperf3 -c 10.7.0.1 -p 5201 -t 10 -P 64

say "Done. Results in $RESULTS"
$DC down 2>&1 | tail -2
