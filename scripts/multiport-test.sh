#!/usr/bin/env bash
# Multi-port concurrent connection pressure test.
#
# Starts N iperf3 servers on the server container across ports 5201..5200+N,
# then fires N concurrent iperf3 clients from the client container (one per
# port), each running for T seconds. Verifies that the tunnel correctly routes
# to arbitrary TCP ports under concurrent load and that every connection
# completes successfully.
#
# Usage: scripts/multiport-test.sh [N] [T]   (defaults N=32 T=8)

set -euo pipefail
cd "$(dirname "$0")/.."
N="${1:-32}"
T="${2:-8}"
BASE=5201

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

# Assumes the kernel-mode stack from load-test.sh is already up; if not, bring
# it up here.
if ! sudo docker ps --format '{{.Names}}' | grep -q '^ripws-server$'; then
    say "Stack not running; starting it"
    bash scripts/load-test.sh >/dev/null 2>&1 || true
fi

say "Starting $N iperf3 servers on ports $BASE..$((BASE+N-1))"
for i in $(seq 0 $((N-1))); do
    port=$((BASE+i))
    sudo docker exec -d ripws-server iperf3 -s -p "$port" -1
done
sleep 1

say "Firing $N concurrent iperf3 clients (each ${T}s)"
pids=()
logs_dir="$(mktemp -d)"
for i in $(seq 0 $((N-1))); do
    port=$((BASE+i))
    sudo docker exec ripws-client iperf3 -c 10.7.0.1 -p "$port" -t "$T" \
        > "$logs_dir/port-$port.log" 2>&1 &
    pids+=($!)
done

fail=0
for pid in "${pids[@]}"; do
    if ! wait "$pid"; then
        fail=$((fail+1))
    fi
done

say "Results: $((N-fail))/$N connections succeeded"
if [ "$fail" -gt 0 ]; then
    echo "Failed ports:"
    grep -l "error" "$logs_dir"/*.log 2>/dev/null | head
fi

say "Aggregate throughput across $N parallel single-stream connections"
# receiver line: "[  5]   0.00-8.00  sec  ...  X.XX Gbits/sec  receiver"
# pull the token just before "receiver" (the bitrate value) and its unit.
grep -h "receiver" "$logs_dir"/*.log \
    | awk '{print $(NF-1), $(NF-2)}' \
    | awk '{s+=$1} END {printf "Total receiver bitrate: %.2f %s\n", s, "Gbits/sec"}'

echo "--- sample log (port $BASE) ---"
sed -n '1,14p' "$logs_dir/port-$BASE.log"
rm -rf "$logs_dir"
