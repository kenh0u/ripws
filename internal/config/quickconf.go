// Package config - wg-quick-style INI configuration for RIPWS.
//
// This parser reads a WireGuard-like INI config file and produces a
// QuickConfig that the `ripws up` / `ripws down` subcommands use to
// automatically bring up the tunnel interface (addresses, routes, MTU,
// DNS, pre/post hooks) — mirroring `wg-quick` convenience.
//
// Field mapping (WireGuard → RIPWS):
//
//	PrivateKey   → PSK (the bearer-token pre-shared key)
//	UUID         → client identity / endpoint path component (replaces PublicKey)
//	PresharedKey → per-peer PSK override (optional)
//	Endpoint     → ws(s):// server URL, host:port only (client only)
//	ListenPort   → server listen port (server only)
//	AllowedIPs   → routes to install via the tunnel
//	PersistentKeepalive → WebSocket Ping interval (seconds)
//	MTU          → interface MTU (default 1420)
//	DNS          → nameserver entries to inject into /etc/resolv.conf
//	Table        → route-table policy: "auto" (default) or "off"
//	PreUp/PostUp/PreDown/PostDown → shell hooks (may repeat)
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// QuickConfig is the parsed wg-quick-style configuration.
type QuickConfig struct {
	Interface InterfaceConfig
	Peers     []PeerConfig
}

// InterfaceConfig holds the [Interface] section values.
type InterfaceConfig struct {
	Address    []string // CIDR addresses to assign (e.g. "10.7.0.1/24")
	ListenPort int      // server listen port (0 = not a server)
	PrivateKey string   // default PSK (base64, 44 chars)
	MTU        int      // interface MTU (0 = default 1420)
	DNS        []string // DNS servers to inject
	Table      string   // "auto" (default) or "off"
	Mode       string   // "kernel" (default) or "userspace"
	NAT        bool     // userspace: enable NAT gateway
	PreUp      []string // pre-up shell hooks
	PostUp     []string // post-up shell hooks
	PreDown    []string // pre-down shell hooks
	PostDown   []string // post-down shell hooks
}

// PeerConfig holds a [Peer] section.
type PeerConfig struct {
	UUID                string   // UUIDv4 of this peer
	PresharedKey        string   // per-peer PSK (optional; overrides Interface PrivateKey)
	AllowedIPs          []string // CIDR routes via tunnel for this peer
	Endpoint            string   // ws(s)://host:port URL (client only, no UUID path)
	PersistentKeepalive int      // WebSocket Ping interval in seconds (0 = default)
}

// DefaultMTU is the default tunnel MTU when not specified in config.
const DefaultMTU = 1420

// LoadQuickConfig reads and parses a wg-quick-style INI file.
func LoadQuickConfig(path string) (*QuickConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ripws: open config %s: %w", path, err)
	}
	defer f.Close()

	cfg := &QuickConfig{}
	cfg.Interface.MTU = 0 // 0 means "use DefaultMTU" later
	cfg.Interface.Table = "auto"
	cfg.Interface.Mode = "kernel"

	var section string
	var currentPeer *PeerConfig

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}

		// Section header
		if line[0] == '[' && line[len(line)-1] == ']' {
			section = strings.TrimSpace(line[1 : len(line)-1])
			switch strings.ToLower(section) {
			case "interface":
				section = "interface"
			case "peer":
				section = "peer"
				cfg.Peers = append(cfg.Peers, PeerConfig{})
				currentPeer = &cfg.Peers[len(cfg.Peers)-1]
			default:
				return nil, fmt.Errorf("ripws: config line %d: unknown section [%s]", lineNo, section)
			}
			continue
		}

		// Key = Value
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("ripws: config line %d: expected Key = Value", lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		key = strings.ToLower(key)

		switch section {
		case "interface":
			if err := cfg.setInterface(key, val, lineNo); err != nil {
				return nil, err
			}
		case "peer":
			if currentPeer == nil {
				return nil, fmt.Errorf("ripws: config line %d: peer field outside [Peer]", lineNo)
			}
			if err := cfg.setPeer(currentPeer, key, val, lineNo); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("ripws: config line %d: key outside any section", lineNo)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ripws: read config: %w", err)
	}

	if cfg.Interface.MTU == 0 {
		cfg.Interface.MTU = DefaultMTU
	}
	if cfg.Interface.Table == "" {
		cfg.Interface.Table = "auto"
	}
	if cfg.Interface.Mode == "" {
		cfg.Interface.Mode = "kernel"
	}

	return cfg, nil
}

func (c *QuickConfig) setInterface(key, val string, lineNo int) error {
	switch key {
	case "address":
		c.Interface.Address = appendCSV(c.Interface.Address, val)
	case "listenport":
		p, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("ripws: config line %d: bad ListenPort %q", lineNo, val)
		}
		c.Interface.ListenPort = p
	case "privatekey", "psk":
		c.Interface.PrivateKey = val
	case "mtu":
		m, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("ripws: config line %d: bad MTU %q", lineNo, val)
		}
		c.Interface.MTU = m
	case "dns":
		c.Interface.DNS = appendCSV(c.Interface.DNS, val)
	case "table":
		c.Interface.Table = strings.ToLower(val)
	case "mode":
		c.Interface.Mode = strings.ToLower(val)
	case "nat":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("ripws: config line %d: bad NAT %q", lineNo, val)
		}
		c.Interface.NAT = b
	case "preup":
		c.Interface.PreUp = append(c.Interface.PreUp, val)
	case "postup":
		c.Interface.PostUp = append(c.Interface.PostUp, val)
	case "predown":
		c.Interface.PreDown = append(c.Interface.PreDown, val)
	case "postdown":
		c.Interface.PostDown = append(c.Interface.PostDown, val)
	case "saveconfig":
		// acknowledged but not implemented
	default:
		return fmt.Errorf("ripws: config line %d: unknown [Interface] key %q", lineNo, key)
	}
	return nil
}

func (c *QuickConfig) setPeer(p *PeerConfig, key, val string, lineNo int) error {
	switch key {
	case "uuid":
		p.UUID = val
	case "presharedkey", "psk":
		p.PresharedKey = val
	case "allowedips":
		p.AllowedIPs = appendCSV(p.AllowedIPs, val)
	case "endpoint":
		p.Endpoint = val
	case "persistentkeepalive":
		k, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("ripws: config line %d: bad PersistentKeepalive %q", lineNo, val)
		}
		p.PersistentKeepalive = k
	default:
		return fmt.Errorf("ripws: config line %d: unknown [Peer] key %q", lineNo, key)
	}
	return nil
}

// appendCSV splits a comma-separated value and appends non-empty parts to dst.
func appendCSV(dst []string, val string) []string {
	for _, p := range strings.Split(val, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			dst = append(dst, p)
		}
	}
	return dst
}

// IsServer returns true when ListenPort is set (server mode).
func (c *QuickConfig) IsServer() bool {
	return c.Interface.ListenPort > 0
}

// IsUserspace returns true when Mode is "userspace" (gVisor netstack, no root).
func (c *QuickConfig) IsUserspace() bool {
	return c.Interface.Mode == "userspace"
}

// InterfaceName derives the interface name from the config file basename,
// stripping the .conf extension (e.g. "ws0.conf" → "ws0").
func InterfaceName(configPath string) string {
	base := configPath
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".conf")
	if base == "" {
		base = "ws0"
	}
	return base
}

// PeerPSK returns the PSK for a peer: the peer's PresharedKey if set,
// otherwise the Interface's PrivateKey (default PSK).
func (c *QuickConfig) PeerPSK(p *PeerConfig) string {
	if p.PresharedKey != "" {
		return p.PresharedKey
	}
	return c.Interface.PrivateKey
}

// AllAllowedIPs returns the union of AllowedIPs from all peers.
func (c *QuickConfig) AllAllowedIPs() []string {
	var out []string
	for _, p := range c.Peers {
		out = append(out, p.AllowedIPs...)
	}
	return out
}
