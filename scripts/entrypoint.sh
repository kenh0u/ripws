#!/bin/sh
# Container entrypoint for the RIPWS test images.
#
# Generates a wg-quick-style INI config from environment variables and runs
# `ripws up` on it. On SIGTERM/SIGINT the process exits and the tunnel is
# cleaned up.
#
# Environment variables:
#   ROLE         server | client
#   MODE         kernel | userspace (default kernel)
#   TUN_ADDR     CIDR assigned to the interface (e.g. 10.7.0.1/24)
#   LISTEN       server: listen port (default 8080)
#   PSK          pre-shared key (base64, 44 chars)
#   UUID         peer UUID (also used as URL path component on client)
#   URL          client: ws(s)://host:port/<uuid> endpoint
#   PING_INTERVAL client: WebSocket Ping interval in seconds (default 60)
#   NAT          userspace: enable NAT gateway (1/0)
#   NAT_ROUTES   server kernel: comma-separated CIDRs to route via tunnel
#   IPERF3       server: start iperf3 -s bound to this address

set -eu

MODE="${MODE:-kernel}"
PING_INTERVAL="${PING_INTERVAL:-60}"
CONF_DIR="/tmp/ripws"
CONF_FILE="$CONF_DIR/ws0.conf"

log() { printf '[entrypoint] %s\n' "$*"; }

mkdir -p "$CONF_DIR"

# Derive the peer's tunnel IP from TUN_ADDR by replacing the last octet with .2
peer_ip() {
    echo "${1:-10.7.0.1/24}" | cut -d/ -f1 | sed 's/\.[0-9]*$/.2/'
}

# Derive the subnet from TUN_ADDR (e.g. 10.7.0.1/24 -> 10.7.0.0/24)
subnet() {
    ip_part=$(echo "${1:-10.7.0.1/24}" | cut -d/ -f1)
    mask=$(echo "${1:-10.7.0.1/24}" | cut -d/ -f2)
    octets=$(echo "$ip_part" | tr '.' ' ')
    set -- $octets
    if [ "$mask" = "24" ]; then
        echo "$1.$2.$3.0/$mask"
    elif [ "$mask" = "16" ]; then
        echo "$1.$2.0.0/$mask"
    else
        echo "$1.$2.$3.0/$mask"
    fi
}

# --- Build INI config from environment ---
build_config() {
    local_addr="${TUN_ADDR:-10.7.0.1/24}"
    tun_subnet=$(subnet "$local_addr")
    log "building config: $CONF_FILE (role=$ROLE mode=$MODE addr=$local_addr)"

    {
        echo "[Interface]"
        echo "Address = $local_addr"
        if [ "$ROLE" = "server" ]; then
            echo "ListenPort = ${LISTEN:-8080}"
        fi
        echo "PrivateKey = $PSK"
        echo "Mode = $MODE"
        if [ "${NAT:-0}" = "1" ]; then
            echo "NAT = true"
        fi
        echo ""
        echo "[Peer]"
        echo "PublicKey = ${UUID}"
        if [ "$ROLE" = "server" ]; then
            # Server: allow the client's /32 plus any NAT target CIDRs.
            client_ip=$(peer_ip "$local_addr")
            allowed="${client_ip}/32"
            if [ -n "${NAT_ROUTES:-}" ]; then
                allowed="${allowed},${NAT_ROUTES}"
            fi
            echo "AllowedIPs = $allowed"
        else
            echo "Endpoint = ${URL}"
            # Client: only route the tunnel subnet via the TUN (avoid stealing
            # the container's default route which would break the WebSocket).
            echo "AllowedIPs = $tun_subnet"
            echo "PersistentKeepalive = ${PING_INTERVAL}"
        fi
    } > "$CONF_FILE"
}

build_config

log "starting: ripws up $CONF_FILE"
ripws up "$CONF_FILE" &
RIPWS_PID=$!

# Optional iperf3 server (server side, after tunnel is up).
if [ "$ROLE" = "server" ] && [ -n "${IPERF3:-}" ]; then
    # Wait briefly for the TUN to come up so iperf3 can bind.
    sleep 2
    log "starting iperf3 -s -B $IPERF3"
    iperf3 -s -B "$IPERF3" -D 2>/dev/null || true
    log "iperf3 server started"
fi

# Wait for ripws to exit and propagate its status.
wait "$RIPWS_PID"
status=$?
log "ripws exited with status $status"
exit $status
