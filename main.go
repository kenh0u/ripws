// Command ripws is a single binary that runs as either a RIPWS server or a
// RIPWS client, selected by the first argument ("server" / "client"). It also
// provides small helpers ("gen-psk", "gen-config") to mint credentials.
//
// RIPWS tunnels raw L3 packets over a WebSocket binary stream; see SPEC.md.
// The data plane runs entirely in user space (two goroutines bridging the
// tunnel interface and the WebSocket), modelled on Tailscale's userspace
// networking architecture.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

	// Top-level subcommand dispatch. Each subcommand owns its own FlagSet so
	// help text stays scoped.
	switch os.Args[1] {
	case "server":
		cmdServer(os.Args[2:])
	case "client":
		cmdClient(os.Args[2:])
	case "up":
		cmdUp(os.Args[2:])
	case "down":
		cmdDown(os.Args[2:])
	case "gen-psk":
		cmdGenPSK(os.Args[2:])
	case "gen-config":
		cmdGenConfig(os.Args[2:])
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
  ripws up   <config.conf>  bring up a tunnel using a wg-quick-style config
  ripws down <config.conf>  tear down a tunnel
  ripws server   [flags]   run the RIPWS server (manual interface config)
  ripws client   [flags]   run the RIPWS client (manual interface config)
  ripws gen-psk             print a fresh 32-byte base64 PSK
  ripws gen-config          print JSON mapping a fresh UUID to a fresh PSK

up / down:
  Uses a wg-quick-style INI config file. The interface name is derived from
  the filename (e.g. ws0.conf -> ws0). Supports Address, ListenPort,
  PrivateKey/PSK, MTU, DNS, Table, PreUp/PostUp/PreDown/PostDown, and
  per-peer PublicKey/UUID, PresharedKey, AllowedIPs, Endpoint,
  PersistentKeepalive. Requires CAP_NET_ADMIN (kernel mode only).

  Example server config (ws0.conf):
    [Interface]
    Address    = 10.7.0.1/24
    ListenPort = 8080
    PrivateKey = <psk>
    MTU        = 1420
    [Peer]
    PublicKey  = <client-uuid>
    AllowedIPs = 10.7.0.2/32

  Example client config (ws0.conf):
    [Interface]
    Address    = 10.7.0.2/24
    PrivateKey = <psk>
    MTU        = 1420
    [Peer]
    PublicKey  = <uuid>
    Endpoint   = ws://server:8080/<uuid>
    AllowedIPs = 0.0.0.0/0
    PersistentKeepalive = 30

server flags:
  --listen ADDR            listen address (default ":8080")
  --config PATH            JSON file mapping UUID -> PSK (repeatable clients)
  --uuid UUID              single-client UUID (mutually ok with --config)
  --psk PSK                single-client PSK
  --mode kernel|userspace  L3 backend (default "kernel")
  --tun-name NAME          kernel: desired TUN name hint (best effort)
  --us-addr IP[,IP...]     userspace: virtual IPs assigned to the netstack NIC
  --us-route CIDR[,CIDR..] userspace: routes via the tunnel NIC (default "0.0.0.0/0,::/0")
  --us-echo ADDR           userspace: start a TCP echo listener bound inside
                           the netstack at this address (e.g. 10.8.0.1:7)
  --ping-window D          read-deadline window for reaping dead clients
                           (default 330s; >= slowest client ping 300s)

client flags:
  --url URL                ws(s)://host:port/<uuid> endpoint
  --psk PSK                pre-shared key (Bearer token)
  --mode kernel|userspace  L3 backend (default "kernel")
  --tun-name NAME          kernel: TUN name hint
  --tun-addr ADDR          kernel: informational; address you will assign to
                           the TUN device (printed at startup)
  --us-addr IP[,IP...]     userspace: local virtual IP(s)
  --us-route CIDR[,CIDR..] userspace: routes via tunnel NIC (default "0.0.0.0/0,::/0")
   --us-dial ADDR           userspace: one-shot echo probe target (e.g. 10.8.0.1:7)
   --us-dial-interval D     repeat the probe at this interval (default 5s)
   --us-ping IP             userspace: send ICMP pings to this IP (e.g. 10.8.0.1)
   --us-ping-count N        number of ping probes (default 4)
   --us-ping-interval D     interval between pings (default 1s)
   --us-forward L=R         userspace: TCP forward LOCAL=REMOTE (e.g. 127.0.0.1:5201=10.8.0.1:5201)
   --us-udp-forward L=R     userspace: UDP forward LOCAL=REMOTE
   --us-nat                 userspace: enable NAT gateway (tunnel traffic proxied out via host net)
   --ping-interval D        WebSocket Ping interval, 30s..300s (default 60s)
  --ping-pong-timeout D    Pong wait before declaring dead (default 15s)
  --backoff-base D         initial reconnect delay (default 500ms)
  --backoff-max D          reconnect delay cap (default 60s)
  --max-retries N          0 = unlimited (default 0)`)
}

// ---------------------------------------------------------------------------
// server
// ---------------------------------------------------------------------------

func cmdServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "listen address")
	configPath := fs.String("config", "", "JSON file mapping UUID -> PSK")
	idFlag := fs.String("uuid", "", "single-client UUID")
	pskFlag := fs.String("psk", "", "single-client PSK")
	mode := fs.String("mode", "kernel", "kernel | userspace")
	tunName := fs.String("tun-name", "", "kernel: TUN name hint")
	usAddr := fs.String("us-addr", "", "userspace: virtual IPs (comma-separated)")
	usRoute := fs.String("us-route", "0.0.0.0/0,::/0", "userspace: routes (comma-separated CIDRs)")
	usEcho := fs.String("us-echo", "", "userspace: start TCP echo listener at ADDR inside netstack")
	pingWindow := fs.Duration("ping-window", 330*time.Second, "read-deadline window")
	mustParse(fs, args)

	creds := config.New()
	if *configPath != "" {
		c, err := config.LoadFile(*configPath)
		must(err)
		creds = c
	}
	if err := config.MergeFlags(creds, *idFlag, *pskFlag); err != nil {
		fatalf("invalid --uuid/--psk: %v", err)
	}
	if len(creds.Keys) == 0 {
		fatalf("no credentials: pass --config and/or --uuid/--psk")
	}

	tun, err := openTunnel(*mode, *tunName, *usAddr, *usRoute)
	must(err)
	defer tun.Close()

	log.Printf("server: L3 backend=%s interface=%s", *mode, tun.Name())
	if *mode == "kernel" {
		log.Printf("server: assign an address to %s and bring it up, e.g.:", tun.Name())
		log.Printf("  ip addr add 10.7.0.1/24 dev %s && ip link set up dev %s", tun.Name(), tun.Name())
	}

	// Optional in-netstack echo service for userspace end-to-end tests.
	if *mode == "userspace" && *usEcho != "" {
		startUserspaceEcho(tun, *usEcho)
	}

	srv := server.New(creds, tun)
	srv.Logger = log.New(os.Stderr, "server: ", log.LstdFlags|log.Lmicroseconds)
	srv.Keep.Window = *pingWindow

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// We never want idle keep-alives hanging around on a tunnel endpoint.
		IdleTimeout: 60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Printf("server: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("server: listening on %s", *listen)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatalf("server: %v", err)
	}
}

// startUserspaceEcho starts a TCP echo listener inside the netstack so that a
// peer (the client, also in userspace mode) can verify end-to-end L3/L4
// connectivity across the tunnel without root or a kernel TUN.
func startUserspaceEcho(t tunnel.Interface, addr string) {
	us, ok := t.(*tunnel.Userspace)
	if !ok {
		log.Printf("server: --us-echo requires --mode=userspace")
		return
	}
	ln, err := us.Listen("tcp", addr)
	if err != nil {
		log.Printf("server: userspace echo listen %s: %v", addr, err)
		return
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c io.ReadWriteCloser) {
				defer c.Close()
				_, _ = io.Copy(c, c) // echo: write back whatever we read
			}(c)
		}
	}()
	log.Printf("server: userspace echo listener on %s", addr)
}

// ---------------------------------------------------------------------------
// client
// ---------------------------------------------------------------------------

func cmdClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	urlFlag := fs.String("url", "", "ws(s)://host:port/<uuid> endpoint")
	pskFlag := fs.String("psk", "", "pre-shared key")
	mode := fs.String("mode", "kernel", "kernel | userspace")
	tunName := fs.String("tun-name", "", "kernel: TUN name hint")
	tunAddr := fs.String("tun-addr", "", "kernel: informational TUN address")
	usAddr := fs.String("us-addr", "", "userspace: local virtual IP(s)")
	usRoute := fs.String("us-route", "0.0.0.0/0,::/0", "userspace: routes (comma CIDRs)")
	 usDial := fs.String("us-dial", "", "userspace: one-shot echo probe target")
	 usDialInterval := fs.Duration("us-dial-interval", 5*time.Second, "probe repeat interval")
	 usPing := fs.String("us-ping", "", "userspace: send ICMP pings to this IP (e.g. 10.8.0.1)")
	 usPingCount := fs.Int("us-ping-count", 4, "userspace: number of ping probes")
	 usPingInterval := fs.Duration("us-ping-interval", 1*time.Second, "userspace: interval between pings")
	 usForward := fs.String("us-forward", "", "userspace: TCP port forward LOCAL=REMOTE (repeatable; e.g. 127.0.0.1:5201=10.8.0.1:5201)")
	 usUDPForward := fs.String("us-udp-forward", "", "userspace: UDP port forward LOCAL=REMOTE")
	 usNAT := fs.Bool("us-nat", false, "userspace: enable NAT gateway — transit traffic from tunnel is proxied out via host networking")
	 pingInterval := fs.Duration("ping-interval", 60*time.Second, "WebSocket Ping interval (30s..300s)")
	pongTimeout := fs.Duration("ping-pong-timeout", 15*time.Second, "Pong wait")
	backoffBase := fs.Duration("backoff-base", 500*time.Millisecond, "initial reconnect delay")
	backoffMax := fs.Duration("backoff-max", 60*time.Second, "reconnect delay cap")
	maxRetries := fs.Int("max-retries", 0, "0 = unlimited")
	mustParse(fs, args)

	if *urlFlag == "" || *pskFlag == "" {
		fatalf("client: --url and --psk are required")
	}
	if err := config.ValidatePSK(*pskFlag); err != nil {
		fatalf("client: %v", err)
	}
	if *pingInterval < 30*time.Second || *pingInterval > 300*time.Second {
		fatalf("client: --ping-interval must be within [30s, 300s]")
	}

	tun, err := openTunnel(*mode, *tunName, *usAddr, *usRoute)
	must(err)
	defer tun.Close()

	log.Printf("client: L3 backend=%s interface=%s", *mode, tun.Name())
	if *mode == "kernel" {
		if *tunAddr != "" {
			log.Printf("client: assign address to %s, e.g.:", tun.Name())
			log.Printf("  ip addr add %s dev %s && ip link set up dev %s", *tunAddr, tun.Name(), tun.Name())
		}
	}

	 // Optional userspace connectivity probe.
	 if *mode == "userspace" && *usDial != "" {
		 startUserspaceProbe(tun, *usDial, *usDialInterval)
	 }

	 // Optional userspace ICMP ping.
	 if *mode == "userspace" && *usPing != "" {
		 startUserspacePing(tun, *usPing, *usPingCount, *usPingInterval)
	 }

	 // Optional userspace TCP/UDP port forwarding.
	 if *mode == "userspace" {
		 if *usForward != "" {
			 startUserspaceForward(tun, *usForward, false)
		 }
		 if *usUDPForward != "" {
			 startUserspaceForward(tun, *usUDPForward, true)
		 }
	 }

	 // Optional userspace NAT gateway: transit traffic from the tunnel is
	 // transparently proxied out through the host's kernel networking.
	 if *mode == "userspace" && *usNAT {
		 us, ok := tun.(*tunnel.Userspace)
		 if !ok {
			 log.Printf("client: --us-nat requires --mode=userspace")
		 } else {
			 if err := us.EnableNAT(log.New(os.Stderr, "client: ", log.LstdFlags|log.Lmicroseconds)); err != nil {
				 log.Printf("client: --us-nat: %v", err)
			 }
		 }
	 }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := client.Config{
		URL:  *urlFlag,
		PSK:  *pskFlag,
		Tun:  tun,
		Keep: proto.KeepAlive{Interval: *pingInterval, PongTimeout: *pongTimeout},
		Backoff: proto.Backoff{
			Base:        *backoffBase,
			Max:         *backoffMax,
			MaxAttempts: *maxRetries,
		},
		Logger: log.New(os.Stderr, "client: ", log.LstdFlags|log.Lmicroseconds),
	}
	if err := client.Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		fatalf("client: %v", err)
	}
}

// startUserspaceProbe periodically dials --us-dial through the netstack and
// echoes a short payload to verify the tunnel carries L3/L4 traffic.
func startUserspaceProbe(t tunnel.Interface, addr string, every time.Duration) {
	us, ok := t.(*tunnel.Userspace)
	if !ok {
		log.Printf("client: --us-dial requires --mode=userspace")
		return
	}
	go func() {
		payload := []byte("ripws-echo\n")
		for {
			time.Sleep(every)
			start := time.Now()
			c, err := us.Dial("tcp", addr)
			if err != nil {
				log.Printf("client: probe %s: dial: %v", addr, err)
				continue
			}
			if _, err := c.Write(payload); err != nil {
				log.Printf("client: probe %s: write: %v", addr, err)
				_ = c.Close()
				continue
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(c, buf); err != nil {
				log.Printf("client: probe %s: read: %v", addr, err)
				_ = c.Close()
				continue
			}
			_ = c.Close()
			if string(buf) == string(payload) {
				log.Printf("client: probe %s OK rtt=%s", addr, time.Since(start))
			} else {
				log.Printf("client: probe %s MISMATCH got %q", addr, buf)
			}
		}
	}()
}

// startUserspacePing sends ICMP echo requests through the netstack and logs
// each result (RTT or error). It runs once and exits.
func startUserspacePing(t tunnel.Interface, dst string, count int, interval time.Duration) {
	us, ok := t.(*tunnel.Userspace)
	if !ok {
		log.Printf("client: --us-ping requires --mode=userspace")
		return
	}
	go func() {
		// Wait briefly for the tunnel to come up.
		time.Sleep(2 * time.Second)
		log.Printf("client: PING %s (%d probes, %s interval)", dst, count, interval)
		ctx := context.Background()
		sent, recv, minRTT, maxRTT, totalRTT := 0, 0, time.Duration(0), time.Duration(0), time.Duration(0)
		for r := range us.Ping(ctx, dst, count, interval) {
			sent++
			if r.Error != nil {
				log.Printf("client: ping seq=%d: %v", r.Seq, r.Error)
			} else {
				recv++
				if minRTT == 0 || r.RTT < minRTT {
					minRTT = r.RTT
				}
				if r.RTT > maxRTT {
					maxRTT = r.RTT
				}
				totalRTT += r.RTT
				log.Printf("client: ping seq=%d rtt=%s", r.Seq, r.RTT)
			}
		}
		loss := 0
		if sent > 0 {
			loss = (sent - recv) * 100 / sent
		}
		avg := time.Duration(0)
		if recv > 0 {
			avg = totalRTT / time.Duration(recv)
		}
		log.Printf("client: ping %s: %d sent, %d recv, %d%% loss, min=%s avg=%s max=%s",
			dst, sent, recv, loss, minRTT, avg, maxRTT)
	}()
}

// startUserspaceForward starts a TCP or UDP port forwarder bridging a kernel
// listener to a remote address through the netstack.
func startUserspaceForward(t tunnel.Interface, spec string, udp bool) {
	us, ok := t.(*tunnel.Userspace)
	if !ok {
		log.Printf("client: --us-forward requires --mode=userspace")
		return
	}
	listen, remote, err := tunnel.ParseForwardSpec(spec)
	if err != nil {
		log.Printf("client: bad forward spec %q: %v", spec, err)
		return
	}
	go func() {
		if udp {
			f := tunnel.NewUDPForwarder(us, listen, remote)
			f.SetLogger(log.New(os.Stderr, "client: ", log.LstdFlags|log.Lmicroseconds))
			if err := f.Serve(); err != nil {
				log.Printf("client: udp-forward %s -> %s: %v", listen, remote, err)
			}
		} else {
			f := tunnel.NewForwarder(us, listen, remote)
			f.SetLogger(log.New(os.Stderr, "client: ", log.LstdFlags|log.Lmicroseconds))
			if err := f.Serve(); err != nil {
				log.Printf("client: forward %s -> %s: %v", listen, remote, err)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// up / down — wg-quick-style convenience subcommands
// ---------------------------------------------------------------------------

// resolveConfigPath converts a bare interface name (e.g. "ws0") into
// /etc/ripws/ws0.conf if the argument has no path separator or .conf suffix.
func resolveConfigPath(arg string) string {
	if strings.ContainsAny(arg, "/") || strings.HasSuffix(arg, ".conf") {
		return arg
	}
	return "/etc/ripws/" + arg + ".conf"
}

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
		if err := creds.Add(peer.PublicKey, psk); err != nil {
			fatalf("up: peer %s: %v", peer.PublicKey, err)
		}
	}

	// Create TUN with the desired name.
	tun, err := tunnel.NewKernel(ifName)
	must(err)
	defer tun.Close()

	log.Printf("up: created interface %s", tun.Name())

	// Auto-configure the interface (addresses, MTU, routes, DNS, hooks).
	setup := &tunnel.InterfaceSetup{
		Name:     tun.Name(),
		Address:  cfg.Interface.Address,
		MTU:      cfg.Interface.MTU,
		Routes:   cfg.AllAllowedIPs(),
		DNS:      cfg.Interface.DNS,
		Table:    cfg.Interface.Table,
		PreUp:    cfg.Interface.PreUp,
		PostUp:   cfg.Interface.PostUp,
		PreDown:  cfg.Interface.PreDown,
		PostDown: cfg.Interface.PostDown,
	}
	must(setup.Up())
	log.Printf("up: interface %s configured (addr=%v mtu=%d routes=%v)", tun.Name(), cfg.Interface.Address, cfg.Interface.MTU, cfg.AllAllowedIPs())

	// Determine server vs client.
	if cfg.IsServer() {
		runUpServer(cfg, creds, tun, setup)
	} else {
		runUpClient(cfg, tun, setup)
	}
}

func runUpServer(cfg *config.QuickConfig, creds *config.Credentials, tun tunnel.Interface, setup *tunnel.InterfaceSetup) {
	listen := fmt.Sprintf(":%d", cfg.Interface.ListenPort)
	srv := server.New(creds, tun)
	srv.Logger = log.New(os.Stderr, "server: ", log.LstdFlags|log.Lmicroseconds)
	srv.Keep.Window = 330 * time.Second

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Printf("up: shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("up: server listening on %s", listen)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatalf("up: server: %v", err)
	}

	log.Printf("up: bringing down interface %s", setup.Name)
	if err := setup.Down(); err != nil {
		log.Printf("up: teardown error: %v", err)
	}
}

func runUpClient(cfg *config.QuickConfig, tun tunnel.Interface, setup *tunnel.InterfaceSetup) {
	if len(cfg.Peers) == 0 {
		fatalf("up: client config requires at least one [Peer] with Endpoint")
	}
	peer := cfg.Peers[0]
	if peer.Endpoint == "" {
		fatalf("up: client [Peer] requires Endpoint = ws(s)://...")
	}
	psk := cfg.PeerPSK(&peer)
	if err := config.ValidatePSK(psk); err != nil {
		fatalf("up: %v", err)
	}

	pingInterval := 60 * time.Second
	if peer.PersistentKeepalive > 0 {
		pingInterval = time.Duration(peer.PersistentKeepalive) * time.Second
	}
	if pingInterval < 30*time.Second || pingInterval > 300*time.Second {
		fatalf("up: PersistentKeepalive must be within [30, 300] seconds")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	clCfg := client.Config{
		URL:  peer.Endpoint,
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
		fatalf("up: client: %v", err)
	}

	log.Printf("up: bringing down interface %s", setup.Name)
	if err := setup.Down(); err != nil {
		log.Printf("up: teardown error: %v", err)
	}
}

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

	setup := &tunnel.InterfaceSetup{
		Name:     ifName,
		PreDown:  cfg.Interface.PreDown,
		PostDown: cfg.Interface.PostDown,
		DNS:      cfg.Interface.DNS,
	}
	if err := setup.Down(); err != nil {
		fatalf("down: %v", err)
	}
	log.Printf("down: interface %s removed", ifName)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// openTunnel builds the requested L3 backend. kernel mode creates a host TUN
// device (requires CAP_NET_ADMIN + /dev/net/tun); userspace mode builds a
// gVisor netstack with the given local IPs and routes (no privileges).
func openTunnel(mode, tunName, usAddr, usRoute string) (tunnel.Interface, error) {
	switch strings.ToLower(mode) {
	case "kernel", "tun":
		return tunnel.NewKernel(tunName)
	case "userspace", "netstack":
		addrs := splitCSV(usAddr)
		routes := parseRoutes(usRoute)
		return tunnel.NewUserspace(addrs, routes)
	default:
		return nil, fmt.Errorf("unknown mode %q (want kernel|userspace)", mode)
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseRoutes(s string) []tunnel.Route {
	parts := splitCSV(s)
	out := make([]tunnel.Route, 0, len(parts))
	for _, p := range parts {
		out = append(out, tunnel.Route{Destination: p})
	}
	return out
}

func cmdGenPSK(args []string) {
	fs := flag.NewFlagSet("gen-psk", flag.ExitOnError)
	mustParse(fs, args)
	b := make([]byte, config.PSKLen)
	if _, err := rand.Read(b); err != nil {
		must(err)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(b))
}

func cmdGenConfig(args []string) {
	fs := flag.NewFlagSet("gen-config", flag.ExitOnError)
	mustParse(fs, args)
	b := make([]byte, config.PSKLen)
	if _, err := rand.Read(b); err != nil {
		must(err)
	}
	id := uuid.New()
	obj := map[string]string{id.String(): base64.StdEncoding.EncodeToString(b)}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		must(err)
	}
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
