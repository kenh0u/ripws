#!/bin/sh
# Container entrypoint for the RIPWS test images.
#
# It starts the ripws binary in the background, waits for the kernel TUN device
# to appear, configures its address / routes, then waits for the ripws process
# to exit. Environment variables:
#
#   ROLE         server | client
#   TUN_ADDR     CIDR assigned to the TUN (e.g. 10.7.0.1/24)
#   TUN_NAME     expected TUN device name (default tun0)
#   LISTEN       server: listen address (default :8080)
#   CONFIG       server: path to JSON credentials (default /cfg/server.json)
#   UUID         server: single-client UUID (optional, with PSK)
#   PSK          server: single-client PSK; client: the PSK to present
#   URL          client: ws(s)://host:port/<uuid>
#   MODE         kernel | userspace (default kernel)
#   US_ADDR      userspace: virtual IP(s)
#   US_ROUTE     userspace: routes
#   US_ECHO      server userspace: echo listener address
#   US_DIAL      client userspace: probe target
#   US_PING      client userspace: ICMP ping target IP
#   US_PING_COUNT client userspace: ping count (default 4)
#   US_FORWARD   client userspace: TCP forward spec LOCAL=REMOTE
#   US_UDP_FORWARD client userspace: UDP forward spec LOCAL=REMOTE
#   US_NAT       client userspace: enable NAT gateway (1/0)
#   NAT_ROUTES   server: comma-separated CIDRs to route via client tunnel IP
#   IPERF3       server: start iperf3 -s bound to this address (e.g. 10.8.0.1)
#   PING_INTERVAL client: ws ping interval (default 60s)

set -eu

TUN_NAME="${TUN_NAME:-tun0}"
MODE="${MODE:-kernel}"

log() { printf '[entrypoint] %s\n' "$*"; }

# water creates a TUN whose name is chosen by the kernel (tun0, tun1, ...).
# We discover the first tun* device that appears after startup.
wait_for_tun() {
    for _ in $(seq 1 100); do
        for cand in /sys/class/net/tun*; do
            [ -e "$cand" ] || continue
            echo "${cand##*/}"
            return 0
        done
        sleep 0.1
    done
    return 1
}

configure_tun() {
    dev="$1"
    log "configuring $dev with $TUN_ADDR"
    ip addr add "$TUN_ADDR" dev "$dev"
    ip link set up dev "$dev"
    # Enable forwarding on the server side (harmless on client).
    if [ "$ROLE" = "server" ]; then
        sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1 || true
    fi
}

start_ripws() {
    case "$ROLE" in
        server)
            args="server --listen ${LISTEN:-:8080} --mode $MODE"
            if [ -n "${CONFIG:-}" ] && [ -f "$CONFIG" ]; then
                args="$args --config $CONFIG"
            fi
            if [ -n "${UUID:-}" ] && [ -n "${PSK:-}" ]; then
                args="$args --uuid $UUID --psk $PSK"
            fi
            if [ -n "${US_ADDR:-}" ]; then args="$args --us-addr $US_ADDR"; fi
            if [ -n "${US_ROUTE:-}" ]; then args="$args --us-route $US_ROUTE"; fi
            if [ -n "${US_ECHO:-}" ]; then args="$args --us-echo $US_ECHO"; fi
            ;;
        client)
            args="client --url $URL --psk $PSK --mode $MODE"
            if [ -n "${US_ADDR:-}" ]; then args="$args --us-addr $US_ADDR"; fi
            if [ -n "${US_ROUTE:-}" ]; then args="$args --us-route $US_ROUTE"; fi
            if [ -n "${US_DIAL:-}" ]; then args="$args --us-dial $US_DIAL"; fi
            if [ -n "${US_PING:-}" ]; then args="$args --us-ping $US_PING"; fi
            if [ -n "${US_PING_COUNT:-}" ]; then args="$args --us-ping-count $US_PING_COUNT"; fi
            if [ -n "${US_FORWARD:-}" ]; then args="$args --us-forward $US_FORWARD"; fi
            if [ -n "${US_UDP_FORWARD:-}" ]; then args="$args --us-udp-forward $US_UDP_FORWARD"; fi
            if [ "${US_NAT:-}" = "1" ]; then args="$args --us-nat"; fi
            args="$args --ping-interval ${PING_INTERVAL:-60s}"
            ;;
        *)
            log "unknown ROLE=$ROLE"; exit 2;;
    esac
    log "starting: ripws $args"
    ripws $args &
    RIPWS_PID=$!
}

start_ripws

if [ "$MODE" = "kernel" ]; then
    dev="$(wait_for_tun)" || { log "TUN did not appear"; exit 1; }
    log "TUN device up: $dev"
    configure_tun "$dev"
fi

# Optional iperf3 server (server side, after TUN is configured).
if [ "$ROLE" = "server" ] && [ -n "${IPERF3:-}" ]; then
    log "starting iperf3 -s -B $IPERF3"
    iperf3 -s -B "$IPERF3" -D 2>/dev/null || true
    sleep 1
    log "iperf3 server started"
fi

# Optional server-side routes via client tunnel IP (for NAT testing).
if [ "$ROLE" = "server" ] && [ "$MODE" = "kernel" ] && [ -n "${NAT_ROUTES:-}" ]; then
    CLIENT_IP="${NAT_CLIENT_IP:-10.7.0.2}"
    IFS=','
    for cidr in $NAT_ROUTES; do
        log "adding route: $cidr via $CLIENT_IP"
        ip route add "$cidr" via "$CLIENT_IP" dev "$TUN_NAME" 2>/dev/null || true
    done
    unset IFS
fi

# Wait for the ripws process to exit, then propagate its status.
wait "$RIPWS_PID"
status=$?
log "ripws exited with status $status"
exit $status
