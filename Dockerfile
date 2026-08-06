# RIPWS test image.
#
# The binary is produced by Nix (`nix build .#ripws` -> result/bin/ripws) for
# full reproducibility. The runtime base is debian-slim with just enough
# networking tooling to assign addresses to the TUN device and run ping.
#
# Build the binary first:
#   nix build .#ripws
# then:
#   docker build -t ripws .

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        iproute2 iputils-ping procps ca-certificates curl jq iperf3 \
 && rm -rf /var/lib/apt/lists/*

# The nix-built static binary is staged into ./bin/ripws by the test script
# (cp -L result/bin/ripws bin/ripws) before `docker build`.
COPY bin/ripws /usr/local/bin/ripws
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/ripws /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
