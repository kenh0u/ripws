#!/usr/bin/env bash
# End-to-end RIPWS L3 ping test over Docker.
#
# Topology (kernel mode):
#
#   ripws-client (tun0 10.7.0.2/24)
#        | WebSocket (ws://server:8080/<uuid>, Bearer PSK)
#   ripws-server (tun0 10.7.0.1/24)
#
# The client pings the server's TUN address; ICMP is carried as raw IP over
# the WebSocket binary stream and answered by the server's kernel.
#
# Requires: nopasswd sudo (for Docker), `nix`, `docker compose`.
# All docker commands are run via sudo; set SUDO if you need a different.

DC="sudo -E docker compose"

set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

# 1. Build the binary reproducibly with Nix.
say "Building ripws with Nix"
nix build .#ripws --no-link
ls -l result/bin/ripws
# Stage the static binary into the build context (nix's `result` symlink points
# into /nix/store, which docker COPY cannot dereference).
mkdir -p bin
rm -f bin/ripws
cp -L result/bin/ripws bin/ripws

# 2. Mint a single client credential (UUID + 44-char base64 PSK).
say "Generating credentials"
CRED_JSON="$(nix run .#ripws -- gen-config)"
echo "$CRED_JSON"
UUID="$(printf '%s' "$CRED_JSON" | jq -r 'keys[0]')"
PSK="$(printf '%s' "$CRED_JSON" | jq -r ".[\"$UUID\"]")"
echo "UUID=$UUID  PSK=$PSK"
mkdir -p config
printf '%s\n' "$CRED_JSON" > config/server.json

# 3. Build the test image (copies the nix binary in).
say "Building Docker image"
${DC} build

# 4. Launch server + client.
say "Starting containers"
export RIPWS_UUID="$UUID"
export RIPWS_PSK="$PSK"
${DC} down --remove-orphans 2>/dev/null || true
${DC} up -d server
sleep 2
${DC} up -d client

# 5. Wait for the client tunnel to come up and ping the server.
say "Waiting for tunnel & running ping"
sleep 3
sudo docker logs ripws-server 2>&1 | tail -8 || true
echo "---"
sudo docker logs ripws-client 2>&1 | tail -8 || true

say "ping 10.7.0.1 from client (over WebSocket tunnel)"
sudo docker exec ripws-client ping -c 4 -W 2 10.7.0.1

say "ping 10.7.0.2 from server (reverse, over tunnel)"
sudo docker exec ripws-server ping -c 4 -W 2 10.7.0.2 || true

# 6. Silent-drop check: a bad UUID / bad PSK must yield a connection reset,
#    NOT an HTTP status code.
say "Silent-drop probe (expect 'Empty reply' / reset, no status line)"
sudo docker exec ripws-client \
  sh -c "curl -sS -i --max-time 5 http://server:8080/00000000-0000-0000-0000-000000000000 \
         -H 'Authorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=' 2>&1 | head -5 || true"

say "Done"
