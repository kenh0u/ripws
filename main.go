// Command ripws tunnels raw L3 packets over a WebSocket binary stream.
//
// Two modes of operation:
//
//   - kernel mode (wg-quick-respect): creates a host TUN device, configures
//     addresses/routes/MTU/DNS via `ip` commands, and bridges it to the
//     WebSocket.  Requires CAP_NET_ADMIN and /dev/net/tun.
//
//   - userspace mode (Tailscale-respect): runs a full L3 stack in user space
//     via gVisor netstack — no TUN device, no CAP_NET_ADMIN.  Optionally
//     enables a NAT gateway so transit traffic from the tunnel is proxied
//     out through the host's kernel networking.
//
// Both modes are driven by a wg-quick-style INI config file and the
// `ripws up` / `ripws down` subcommands.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"ripws/internal/client"
	"ripws/internal/config"
	"ripws/internal/proto"
	"ripws/internal/server"
	"ripws/internal/tunnel"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "up":
		cmdUp(os.Args[2:])
	case "down":
		cmdDown(os.Args[2:])
	case "genkey":
		cmdGenKey(os.Args[2:])
	case "genuuid":
		cmdGenUUID(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `ripws - Raw IP over WebSocket

usage:
  ripws up   <config.conf>  bring up a tunnel
  ripws down <config.conf>  tear down a tunnel
  ripws genkey             print a fresh 32-byte base64 PSK
  ripws genuuid            print a fresh UUIDv4

up / down:
  Uses a wg-quick-style INI config file. The interface name is derived from
  the filename (e.g. ws0.conf -> ws0). A bare name (e.g. "ws0") resolves to
  /etc/ripws/ws0.conf.

  [Interface] section:
    Address    = CIDR       (repeatable) e.g. 10.7.0.1/24
    ListenPort = N          server listen port (omit for client)
    PrivateKey = PSK        base64 32-byte pre-shared key
    MTU        = N          default 1420
    DNS        = IP         (repeatable) inject into resolv.conf
    Table      = auto|off   route-table policy (default auto)
    Mode       = kernel|userspace   L3 backend (default kernel)
    NAT        = true|false userspace: enable NAT gateway (default false)
    PreUp/PostUp/PreDown/PostDown = shell hooks (repeatable)

  [Peer] section (repeatable):
    UUID       = UUIDv4     client identity / endpoint path
    PresharedKey = PSK      per-peer PSK (optional; overrides PrivateKey)
    AllowedIPs = CIDR       (repeatable) routes via tunnel
    Endpoint   = URL        client: ws(s)://host:port
    PersistentKeepalive = N seconds (WebSocket Ping interval, 30..300)

  kernel mode requires CAP_NET_ADMIN + /dev/net/tun.
  userspace mode requires no privileges.

  Example server (ws0.conf):
    [Interface]
    Address    = 10.7.0.1/24
    ListenPort = 8080
    PrivateKey = <psk>
    [Peer]
    UUID       = <client-uuid>
    AllowedIPs = 10.7.0.2/32

  Example client (ws0.conf):
    [Interface]
    Address    = 10.7.0.2/24
    PrivateKey = <psk>
    [Peer]
    UUID       = <uuid>
    Endpoint   = ws://server:8080
    AllowedIPs = 0.0.0.0/0
    PersistentKeepalive = 30

  Example userspace client with NAT (ws0.conf):
    [Interface]
    Address = 10.7.0.2/24
    PrivateKey = <psk>
    Mode    = userspace
    NAT     = true
    [Peer]
    UUID       = <uuid>
    Endpoint   = ws://server:8080
    AllowedIPs = 0.0.0.0/0
    PersistentKeepalive = 30`)
}

// ---------------------------------------------------------------------------
// up — bring up a tunnel
// ---------------------------------------------------------------------------

func cmdUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	mustParse(fs, args)
	if fs.NArg() < 1 {
		fatalf("up: config file required (e.g. ripws up ws0.conf)")
	}
	path := resolveConfigPath(fs.Arg(0))

	cfg, err := config.LoadQuickConfig(path)
	must(err)

	ifName := config.InterfaceName(path)

	// Build credentials from [Peer] sections.
	creds := config.New()
	for _, peer := range cfg.Peers {
		psk := cfg.PeerPSK(&peer)
		if err := creds.Add(peer.UUID, psk); err != nil {
			fatalf("up: peer %s: %v", peer.UUID, err)
		}
	}

	// Write PID file so `ripws down` can find us.
	pidPath := pidFilePath(ifName)
	_ = os.MkdirAll(filepath.Dir(pidPath), 0755)
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644); err != nil {
		log.Printf("up: warning: cannot write PID file %s: %v", pidPath, err)
	}
	defer os.Remove(pidPath)

	// Signal handler: SIGTERM and SIGINT both trigger clean shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	proxyRules, err := buildProxyRules(cfg)
	must(err)

	if cfg.IsUserspace() {
		must(runUpUserspace(ctx, cfg, creds, ifName, proxyRules))
	} else {
		runUpKernel(ctx, cfg, creds, ifName, proxyRules)
	}
}

// buildProxyRules converts config [ProxyRule] sections into tunnel rules,
// validating CIDRs and proxy URLs at load time.
func buildProxyRules(cfg *config.QuickConfig) ([]*tunnel.ProxyRule, error) {
	if len(cfg.ProxyRules) == 0 {
		return nil, nil
	}
	rules := make([]*tunnel.ProxyRule, 0, len(cfg.ProxyRules))
	for i := range cfg.ProxyRules {
		r := &tunnel.ProxyRule{
			CIDRs: cfg.ProxyRules[i].CIDRs,
			Proxy: cfg.ProxyRules[i].Proxy,
			SNI:   cfg.ProxyRules[i].SNI,
		}
		if err := r.Validate(); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// wrapSplit wraps tun with the in-process proxy dataplane when proxy rules
// are configured; otherwise it returns tun unchanged (zero regression).
func wrapSplit(tun tunnel.Interface, cfg *config.QuickConfig, rules []*tunnel.ProxyRule) tunnel.Interface {
	if len(rules) == 0 {
		return tun
	}
	localAddrs := make([]string, 0, len(cfg.Interface.Address))
	for _, a := range cfg.Interface.Address {
		if ip, _, err := net.ParseCIDR(a); err == nil {
			localAddrs = append(localAddrs, ip.String())
		} else {
			localAddrs = append(localAddrs, a)
		}
	}
	split, err := tunnel.NewSplit(tun, localAddrs, rules, cfg.Interface.MTU,
		log.New(os.Stderr, "proxy: ", log.LstdFlags|log.Lmicroseconds))
	if err != nil {
		fatalf("up: proxy rules: %v", err)
	}
	log.Printf("up: split dataplane active (%d proxy rule(s))", len(rules))
	return split
}

// runUpKernel brings up a kernel-mode tunnel (TUN + wg-quick-style config).
func runUpKernel(ctx context.Context, cfg *config.QuickConfig, creds *config.Credentials, ifName string, proxyRules []*tunnel.ProxyRule) {
	tun, err := tunnel.NewKernel(ifName)
	must(err)
	defer tun.Close()

	log.Printf("up: created interface %s", tun.Name())

	setup := &tunnel.InterfaceSetup{
		Name:     tun.Name(),
		Address:  cfg.Interface.Address,
		MTU:      cfg.Interface.MTU,
		Routes:   cfg.AllAllowedIPs(),
		DNS:      cfg.Interface.DNS,
		Table:    cfg.Interface.Table,
		Endpoint: endpointHost(cfg),
		PreUp:    cfg.Interface.PreUp,
		PostUp:   cfg.Interface.PostUp,
		PreDown:  cfg.Interface.PreDown,
		PostDown: cfg.Interface.PostDown,
	}
	must(setup.Up())
	log.Printf("up: interface %s configured (addr=%v mtu=%d routes=%v)",
		tun.Name(), cfg.Interface.Address, cfg.Interface.MTU, cfg.AllAllowedIPs())

	// Ensure teardown runs even on fatal exit.
	defer func() {
		log.Printf("up: bringing down interface %s", setup.Name)
		if err := setup.Down(); err != nil {
			log.Printf("up: teardown error: %v", err)
		}
	}()

	// Split dataplane: proxy-rule CIDRs bypass the kernel TUN entirely.
	split := wrapSplit(tun, cfg, proxyRules)

	if cfg.IsServer() {
		must(runServer(ctx, cfg, creds, split))
	} else {
		must(runClient(ctx, cfg, split))
	}
}

// runUpUserspace brings up a userspace-mode tunnel (gVisor netstack, no root).
func runUpUserspace(ctx context.Context, cfg *config.QuickConfig, creds *config.Credentials, ifName string, proxyRules []*tunnel.ProxyRule) error {
	// Strip CIDR suffixes — NewUserspace takes plain IPs.
	localAddrs := make([]string, 0, len(cfg.Interface.Address))
	for _, a := range cfg.Interface.Address {
		if ip, _, err := net.ParseCIDR(a); err == nil {
			localAddrs = append(localAddrs, ip.String())
		} else {
			localAddrs = append(localAddrs, a)
		}
	}

	// Build routes from peer AllowedIPs.
	routes := make([]tunnel.Route, 0)
	for _, cidr := range cfg.AllAllowedIPs() {
		routes = append(routes, tunnel.Route{Destination: cidr})
	}

	tun, err := tunnel.NewUserspaceMTU(localAddrs, routes, cfg.Interface.MTU)
	must(err)
	defer tun.Close()

	log.Printf("up: userspace netstack ready (addrs=%v routes=%v)", localAddrs, cfg.AllAllowedIPs())

	// Optional NAT gateway, or split-proxy rules (which subsume NAT: the
	// proxy dialer replaces net.Dial for matched CIDRs).
	if len(proxyRules) > 0 {
		matcherRules := proxyRules
		if err := tun.EnableNATWithRules(
			log.New(os.Stderr, "proxy: ", log.LstdFlags|log.Lmicroseconds),
			matcherRules, nil, nil); err != nil {
			return fmt.Errorf("up: proxy rules: %w", err)
		}
	} else if cfg.Interface.NAT {
		if err := tun.EnableNAT(log.New(os.Stderr, "nat: ", log.LstdFlags|log.Lmicroseconds)); err != nil {
			log.Printf("up: NAT: %v", err)
		}
	}

	if cfg.IsServer() {
		return runServer(ctx, cfg, creds, tun)
	}
	return runClient(ctx, cfg, tun)
}

// endpointHost extracts the host portion of the first peer's Endpoint URL, so
// a host route can be pinned outside the tunnel before the default route
// moves into the VPN. Empty on the server.
func endpointHost(cfg *config.QuickConfig) string {
	if cfg.IsServer() || len(cfg.Peers) == 0 || cfg.Peers[0].Endpoint == "" {
		return ""
	}
	u, err := url.Parse(cfg.Peers[0].Endpoint)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		return "" // DNS name: resolved later by the client dialer; cannot pin
	}
	return host
}

// runServer starts the HTTP server that accepts WebSocket tunnels.
func runServer(ctx context.Context, cfg *config.QuickConfig, creds *config.Credentials, tun tunnel.Interface) error {
	listen := fmt.Sprintf(":%d", cfg.Interface.ListenPort)
	srv := server.New(creds, tun)
	srv.Logger = log.New(os.Stderr, "server: ", log.LstdFlags|log.Lmicroseconds)
	srv.Keep.Window = 330 * time.Second
	srv.MaxSessions = 1024

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Printf("up: shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("up: server listening on %s", listen)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("up: server: %w", err)
	}
	return nil
}

// runClient starts the WebSocket client that connects to the server.
func runClient(ctx context.Context, cfg *config.QuickConfig, tun tunnel.Interface) error {
	if len(cfg.Peers) == 0 {
		return fmt.Errorf("up: client config requires at least one [Peer] with Endpoint")
	}
	peer := cfg.Peers[0]
	if peer.Endpoint == "" {
		return fmt.Errorf("up: client [Peer] requires Endpoint = ws(s)://host:port")
	}
	if peer.UUID == "" {
		return fmt.Errorf("up: client [Peer] requires UUID")
	}
	psk := cfg.PeerPSK(&peer)
	if err := config.ValidatePSK(psk); err != nil {
		return fmt.Errorf("up: %w", err)
	}

	// Construct the full WebSocket URL: endpoint + "/" + uuid
	wsURL := strings.TrimRight(peer.Endpoint, "/") + "/" + peer.UUID

	pingInterval := 60 * time.Second
	if peer.PersistentKeepalive > 0 {
		pingInterval = time.Duration(peer.PersistentKeepalive) * time.Second
	}
	if pingInterval < 30*time.Second || pingInterval > 300*time.Second {
		return fmt.Errorf("up: PersistentKeepalive must be within [30, 300] seconds")
	}

	clCfg := client.Config{
		URL:  wsURL,
		PSK:  psk,
		Tun:  tun,
		Keep: proto.KeepAlive{Interval: pingInterval, PongTimeout: 15 * time.Second},
		Backoff: proto.Backoff{
			Base: 500 * time.Millisecond,
			Max:  60 * time.Second,
		},
		Logger: log.New(os.Stderr, "client: ", log.LstdFlags|log.Lmicroseconds),
	}
	if err := client.Run(ctx, clCfg); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("up: client: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// down — tear down a tunnel
// ---------------------------------------------------------------------------

func cmdDown(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	mustParse(fs, args)
	if fs.NArg() < 1 {
		fatalf("down: config file required (e.g. ripws down ws0.conf)")
	}
	path := resolveConfigPath(fs.Arg(0))

	cfg, err := config.LoadQuickConfig(path)
	must(err)

	ifName := config.InterfaceName(path)
	pidPath := pidFilePath(ifName)

	// 1. Try to stop a running `ripws up` process via SIGTERM.
	if data, err := os.ReadFile(pidPath); err == nil {
		var pid int
		fmt.Sscanf(string(data), "%d", &pid)
		if pid > 0 {
			if comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil && strings.TrimSpace(string(comm)) == "ripws" {
				proc, err := os.FindProcess(pid)
				if err == nil {
					if err := proc.Signal(syscall.SIGTERM); err == nil {
						log.Printf("down: sent SIGTERM to pid %d (%s)", pid, ifName)
						// Wait for the process to exit.
						for i := 0; i < 50; i++ {
							if proc.Signal(syscall.Signal(0)) != nil {
								break // process is gone
							}
							time.Sleep(100 * time.Millisecond)
						}
					}
				}
			} else {
				log.Printf("down: pid %d is not a ripws process (comm mismatch or gone); skipping signal", pid)
			}
		}
		os.Remove(pidPath)
	}

	// 2. If kernel mode, clean up the interface in case the process is dead.
	if !cfg.IsUserspace() {
		// Check if the interface still exists.
		if err := exec.Command("ip", "link", "show", ifName).Run(); err == nil {
			setup := &tunnel.InterfaceSetup{
				Name:     ifName,
				PreDown:  cfg.Interface.PreDown,
				PostDown: cfg.Interface.PostDown,
				DNS:      cfg.Interface.DNS,
			}
			if err := setup.Down(); err != nil {
				log.Printf("down: %v", err)
			} else {
				log.Printf("down: interface %s removed", ifName)
			}
		} else {
			log.Printf("down: interface %s does not exist (already clean)", ifName)
		}
	} else {
		log.Printf("down: userspace mode — process stopped, nothing to clean up")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// resolveConfigPath converts a bare interface name (e.g. "ws0") into
// /etc/ripws/ws0.conf if the argument has no path separator or .conf suffix.
func resolveConfigPath(arg string) string {
	if strings.ContainsAny(arg, "/") || strings.HasSuffix(arg, ".conf") {
		return arg
	}
	return "/etc/ripws/" + arg + ".conf"
}

// pidFilePath returns the PID file path for a given interface name. Uses
// /run/ripws (root-owned, created 0700) when writable so an unprivileged
// user cannot poison or hijack another user's PID file; falls back to
// /tmp/ripws-<if>.pid for unprivileged runs.
func pidFilePath(ifName string) string {
	dir := "/run/ripws"
	if err := os.MkdirAll(dir, 0700); err == nil {
		if f, err := os.OpenFile(dir+"/.probe", os.O_CREATE|os.O_WRONLY, 0600); err == nil {
			f.Close()
			os.Remove(dir + "/.probe")
			return filepath.Join(dir, ifName+".pid")
		}
	}
	return filepath.Join("/tmp", "ripws-"+ifName+".pid")
}

func cmdGenKey(args []string) {
	fs := flag.NewFlagSet("genkey", flag.ExitOnError)
	mustParse(fs, args)
	b := make([]byte, config.PSKLen)
	if _, err := rand.Read(b); err != nil {
		must(err)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(b))
}

func cmdGenUUID(args []string) {
	fs := flag.NewFlagSet("genuuid", flag.ExitOnError)
	mustParse(fs, args)
	fmt.Println(uuid.New())
}

func mustParse(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
}

func must(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	log.Printf("fatal: "+format, args...)
	os.Exit(1)
}
