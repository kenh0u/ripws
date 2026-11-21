package tunnel

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// ProxyRule describes one intercepted destination range: packets whose
// destination IP falls inside CIDRs are terminated in-process and carried
// through the upstream Proxy URL instead of the normal tunnel datapath.
type ProxyRule struct {
	CIDRs []string // destination CIDRs (IPv4/IPv6)
	Proxy string   // http(s):// or socks5(h):// proxy URL, optional user:pass@
	SNI   bool     // peek TLS ClientHello and CONNECT <sni>:<port> instead of <dstIP>:<port>
}

// ValidProxySchemes are the accepted [ProxyRule] Proxy URL schemes.
var ValidProxySchemes = []string{"http", "https", "socks5", "socks5h"}

// Validate checks a rule: at least one CIDR, all parseable via netip; a
// proxy URL with a known scheme and a host.
func (r *ProxyRule) Validate() error {
	if len(r.CIDRs) == 0 {
		return fmt.Errorf("ripws: proxy rule needs at least one CIDR")
	}
	for _, c := range r.CIDRs {
		if _, err := netip.ParsePrefix(c); err != nil {
			return fmt.Errorf("ripws: bad CIDR %q: %w", c, err)
		}
	}
	scheme := r.proxyScheme()
	for _, ok := range ValidProxySchemes {
		if scheme == ok {
			return nil
		}
	}
	return fmt.Errorf("ripws: bad proxy URL %q (scheme must be one of %s)",
		r.Proxy, strings.Join(ValidProxySchemes, ", "))
}

func (r *ProxyRule) proxyScheme() string {
	i := strings.Index(r.Proxy, "://")
	if i < 0 {
		return ""
	}
	return strings.ToLower(r.Proxy[:i])
}

type proxyEntry struct {
	prefix netip.Prefix
	rule   *ProxyRule
}

// proxyMatcher compiles proxy rules into a longest-prefix-match table.
type proxyMatcher struct {
	// entries sorted by descending prefix bits; stable sort preserves
	// first-configured order among equal lengths.
	entries []proxyEntry
}

func newProxyMatcher(rules []*ProxyRule) (*proxyMatcher, error) {
	m := &proxyMatcher{}
	for _, r := range rules {
		for _, c := range r.CIDRs {
			p, err := netip.ParsePrefix(c)
			if err != nil {
				return nil, err
			}
			m.entries = append(m.entries, proxyEntry{
				prefix: p.Masked(),
				rule:   r,
			})
		}
	}
	slices.SortStableFunc(m.entries, func(a, b proxyEntry) int {
		return cmp.Compare(b.prefix.Bits(), a.prefix.Bits())
	})
	return m, nil
}

// Match returns the rule for dst via longest prefix match, or nil.
func (m *proxyMatcher) Match(dst netip.Addr) *ProxyRule {
	dst = dst.Unmap()
	for _, e := range m.entries {
		if e.prefix.Contains(dst) {
			return e.rule
		}
	}
	return nil
}
