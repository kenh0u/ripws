//go:build linux

package tunnel

import (
	"fmt"
	"net/netip"
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

// proxyMatcher compiles proxy rules into a longest-prefix-match table.
type proxyMatcher struct {
	// prefixes sorted by descending bits; within the same length the first
	// configured rule wins (scan order).
	prefixes []netip.Prefix
	rules    []*ProxyRule
}

func newProxyMatcher(rules []*ProxyRule) (*proxyMatcher, error) {
	m := &proxyMatcher{rules: rules}
	for _, r := range rules {
		for _, c := range r.CIDRs {
			p, err := netip.ParsePrefix(c)
			if err != nil {
				return nil, err
			}
			m.prefixes = append(m.prefixes, p.Masked())
		}
	}
	// Sort by prefix length descending (longest prefix match); a stable sort
	// keeps first-configured order among equal lengths.
	for i := 0; i < len(m.prefixes); i++ {
		for j := i + 1; j < len(m.prefixes); j++ {
			if m.prefixes[j].Bits() > m.prefixes[i].Bits() {
				m.prefixes[i], m.prefixes[j] = m.prefixes[j], m.prefixes[i]
				m.rules[i], m.rules[j] = m.rules[j], m.rules[i]
			}
		}
	}
	return m, nil
}

// Match returns the rule for dst via longest prefix match, or nil.
func (m *proxyMatcher) Match(dst netip.Addr) *ProxyRule {
	for i, p := range m.prefixes {
		if p.Contains(dst) {
			return m.rules[i]
		}
	}
	return nil
}
