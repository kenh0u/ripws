package tunnel

import (
	"fmt"
	"net/netip"
	"strings"
)

// parseOpenBSDAddress normalizes an address entry (CIDR or plain IP) for
// `ifconfig <tun> inet[6] <addr> alias`, preserving the host IP and returning
// the bare IP string for later `route -iface` calls.
func parseOpenBSDAddress(raw string) (cidrArg, ipOnly string, isV6 bool, err error) {
	s := strings.TrimSpace(raw)
	if pfx, perr := netip.ParsePrefix(s); perr == nil {
		addr := pfx.Addr().Unmap()
		norm := netip.PrefixFrom(addr, pfx.Bits())
		return norm.String(), addr.String(), addr.Is6(), nil
	}
	if addr, aerr := netip.ParseAddr(s); aerr == nil {
		addr = addr.Unmap()
		norm := netip.PrefixFrom(addr, addr.BitLen())
		return norm.String(), addr.String(), addr.Is6(), nil
	}
	return "", "", false, fmt.Errorf("ripws: bad address %q", raw)
}

// expandOpenBSDRoutes converts cidr into masked route target(s). Default routes
// (0.0.0.0/0 and ::/0) are split into the two covering /1 halves (0.0.0.0/1 +
// 128.0.0.0/1, ::/1 + 8000::/1) so the host's original default route stays
// intact underneath the tunnel.
func expandOpenBSDRoutes(cidr string) (masked netip.Prefix, targets []netip.Prefix, err error) {
	pfx, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return netip.Prefix{}, nil, fmt.Errorf("ripws: bad route CIDR %q: %w", cidr, err)
	}
	addr := pfx.Addr().Unmap()
	masked = netip.PrefixFrom(addr, pfx.Bits()).Masked()
	if masked.Bits() == 0 {
		if addr.Is6() {
			return masked, []netip.Prefix{
				netip.MustParsePrefix("::/1"),
				netip.MustParsePrefix("8000::/1"),
			}, nil
		}
		return masked, []netip.Prefix{
			netip.MustParsePrefix("0.0.0.0/1"),
			netip.MustParsePrefix("128.0.0.0/1"),
		}, nil
	}
	return masked, []netip.Prefix{masked}, nil
}

// openBSDHostCIDR returns a /32 or /128 host prefix for addr.
func openBSDHostCIDR(addr netip.Addr) string {
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()).String()
}

// parseOpenBSDRouteGet extracts the gateway IP (if any) and egress interface
// name from OpenBSD `route -n get` output. Non-IP gateways (such as link#N)
// yield an empty gateway string.
func parseOpenBSDRouteGet(out []byte) (gateway, iface string) {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "gateway:"):
			gw := strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
			if a, err := netip.ParseAddr(gw); err == nil {
				gateway = a.String()
			}
		case strings.HasPrefix(line, "interface:"):
			iface = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	return gateway, iface
}
