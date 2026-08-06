#!/usr/bin/env bash
# Userspace-mode (gVisor netstack) end-to-end test.
#
# Neither container needs a kernel TUN for the data plane; L3 is handled
# entirely in user space by gVisor's netstack. The server runs a TCP echo
# listener inside its netstack at 10.8.0.1:7; the client's netstack dials it
# through the WebSocket tunnel every 2s and verifies the echo.

set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$PWD"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

say "Building ripws with Nix"
nix build .#ripws --no-link
mkdir -p bin && rm -f bin/ripws && cp -L result/bin/ripws bin/ripws

say "Building Docker image"
sudo -E docker compose build

say "Generating credentials"
CRED_JSON="$(nix run .#ripws -- gen-config)"
UUID="$(printf '%s' "$CRED_JSON" | jq -r 'keys[0]')"
PSK="$(printf '%s' "$CRED_JSON" | jq -r ".[\"$UUID\"]")"
echo "UUID=$UUID  PSK=$PSK"

say "Starting userspace containers (no TUN data plane)"
sudo docker network create ripws-us 2>/dev/null || true
sudo docker rm -f ripws-us-server ripws-us-client 2>/dev/null || true
sudo docker run -d --name ripws-us-server --network ripws-us \
  -e ROLE=server -e MODE=userspace -e LISTEN=:8080 \
  -e UUID="$UUID" -e PSK="$PSK" \
  -e US_ADDR=10.8.0.1 -e US_ROUTE=10.8.0.0/24 -e US_ECHO=10.8.0.1:7 \
  ripws
sleep 1
sudo docker run -d --name ripws-us-client --network ripws-us \
  -e ROLE=client -e MODE=userspace \
  -e URL="ws://ripws-us-server:8080/$UUID" -e PSK="$PSK" \
  -e US_ADDR=10.8.0.2 -e US_ROUTE=10.8.0.0/24 \
  -e US_DIAL=10.8.0.1:7 -e US_DIAL_INTERVAL=2s \
  ripws

say "Waiting for probes"
sleep 8
echo "--- server ---"; sudo docker logs ripws-us-server 2>&1 | tail -6
echo "--- client ---"; sudo docker logs ripws-us-client 2>&1 | tail -8

if sudo docker logs ripws-us-client 2>&1 | grep -q "probe 10.8.0.1:7 OK"; then
    say "USERSPACE ECHO OK"
else
    say "USERSPACE ECHO FAILED"; exit 1
fi

say "Cleanup"
sudo docker rm -f ripws-us-server ripws-us-client >/dev/null
sudo docker network rm ripws-us >/dev/null 2>&1 || true
