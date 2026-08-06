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
  ripws server   [flags]   run the RIPWS server
  ripws client   [flags]   run the RIPWS client
  ripws gen-psk             print a fresh 32-byte base64 PSK
  ripws gen-config          print JSON mapping a fresh UUID to a fresh PSK

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

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// openTunnel builds the requested L3 backend. kernel mode creates a host TUN
// device (requires CAP_NET_ADMIN + /dev/net/tun); userspace mode builds a
// gVisor netstack with the given local IPs and routes (no privileges).
func openTunnel(mode, tunName, usAddr, usRoute string) (tunnel.Interface, error) {
	switch strings.ToLower(mode) {
	case "kernel", "tun":
		return tunnel.NewKernel()
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
