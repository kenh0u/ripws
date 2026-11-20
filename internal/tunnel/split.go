//go:build linux

// Split-Dataplane: route selected destination CIDRs through an in-process
// gVisor netstack that carries them to an upstream HTTP CONNECT / SOCKS5
// proxy, while everything else keeps flowing through the primary (kernel
// TUN) interface unchanged — one WebSocket, one server TUN, zero kernel
// policy for the intercepted ranges.
//
//	WS (raw IP packets)
//	  │
//	  ├─ dst ∈ ProxyRule.CIDRs ──▶ in-process gVisor netstack
//	  │                              └─ (optional TLS SNI peek) ─▶ proxy
//	  │                              └─ UDP: refused (proxies carry TCP)
//	  │
//	  └─ everything else ────────▶ primary interface (kernel TUN)
package tunnel

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// NewSplit wraps primary (kernel TUN) with a userspace proxy dataplane for
// the given rules. It returns primary unchanged when rules is empty, so
// configs without [ProxyRule] keep the exact previous behaviour.
//
// The embedded netstack uses default routes (0.0.0.0/0 + ::/0) so it accepts
// arbitrary tunnel-destination addresses; the actual scope is enforced by
// the matcher on WritePacket, not by the netstack's route table.
//
// Data flow inside the netstack: a TCP connect from the far end of the
// tunnel arrives as an injected SYN; the stack routes it out its only NIC
// (the channel link) because no local endpoint owns the destination. The
// netstack goroutine (start below) reads those egress packets and re-injects
// them as INBOUND — that is the loopback trick that makes the stack treat
// the proxy-ruled destination as "on the other side of the NIC", where the
// forwarder (EnableNATWithRules) picks up the SYN and terminates the flow
// at the upstream proxy.
func NewSplit(primary Interface, localAddrs []string, rules []*ProxyRule, mtu int, logger *log.Logger) (Interface, error) {
	if len(rules) == 0 {
		return primary, nil
	}
	m, err := newProxyMatcher(rules)
	if err != nil {
		return nil, err
	}
	us, err := NewUserspaceMTU(localAddrs, []Route{
		{Destination: "0.0.0.0/0"},
		{Destination: "::/0"},
	}, mtu)
	if err != nil {
		return nil, err
	}
	split := &SplitInterface{primary: primary, userspace: us, match: m}
	split.start() // allocates pktCh and starts the primary pump
	if err := us.EnableNATWithRules(logger, rules, m, split.pktCh); err != nil {
		us.Close()
		split.Close()
		return nil, err
	}
	return split, nil
}

// SplitInterface fans packets out to the primary interface or an in-process
// proxy netstack, and fans their egress back into a single reader channel.
// It implements tunnel.Interface.
type SplitInterface struct {
	primary   Interface
	userspace *Userspace
	match     *proxyMatcher

	once   sync.Once
	pktCh  chan []byte
	subCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ Interface = (*SplitInterface)(nil)

// dstAddr extracts the destination IP without allocating.
func dstAddr(p []byte) (netip.Addr, bool) {
	switch p[0] >> 4 {
	case 4:
		if len(p) < 20 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte(p[16:20])), true
	case 6:
		if len(p) < 40 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16([16]byte(p[24:40])), true
	}
	return netip.Addr{}, false
}

// WritePacket dispatches one inbound (from the WebSocket) IP packet:
// proxy-rule destinations go to the in-process netstack, everything else to
// the primary interface.
func (s *SplitInterface) WritePacket(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, errors.New("ripws: empty packet")
	}
	if dst, ok := dstAddr(p); ok && s.match.Match(dst) != nil {
		return s.userspace.WritePacket(p)
	}
	return s.primary.WritePacket(p)
}

// ReadPacket blocks until either dataplane emits a packet.
func (s *SplitInterface) ReadPacket(p []byte) (int, error) {
	return s.ReadPacketCtx(context.Background(), p)
}

// start lazily launches the fan-in goroutines: one pumping the primary
// interface, one closing the netstack loop (egress → inbound re-injection).
func (s *SplitInterface) start() {
	s.once.Do(func() {
		s.pktCh = make(chan []byte, 64)
		s.subCtx, s.cancel = context.WithCancel(context.Background())
		s.wg.Add(1)
		go s.pump(s.primary)
	})
}

// pump reads packets from one dataplane and forwards them to pktCh. The
// borrowed MTU buffer becomes the caller's property (copy on receive).
func (s *SplitInterface) pump(ifc Interface) {
	defer s.wg.Done()
	defer s.cancel() // either side dying ends the whole interface
	buf := make([]byte, MTU)
	for {
		n, err := ifc.ReadPacketCtx(s.subCtx, buf)
		if err != nil {
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		select {
		case s.pktCh <- pkt:
		case <-s.subCtx.Done():
			return
		}
	}
}

// ReadPacketCtx is ReadPacket with cancellation.
func (s *SplitInterface) ReadPacketCtx(ctx context.Context, p []byte) (int, error) {
	s.start()
	select {
	case pkt := <-s.pktCh:
		n := copy(p, pkt)
		return n, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Name mirrors the primary interface.
func (s *SplitInterface) Name() string { return s.primary.Name() }

// Close shuts both dataplanes down and waits for the pumps. Idempotent.
func (s *SplitInterface) Close() error {
	s.start() // ensure cancel exists even if never read
	s.cancel()
	errP := s.primary.Close()
	errU := s.userspace.Close()
	s.wg.Wait()
	if errP != nil {
		return errP
	}
	return errU
}

// EnableNATWithRules installs TCP and UDP forwarders that carry matched
// traffic to the configured upstream proxies instead of net.Dial.
//
// UDP: HTTP(S) proxies cannot carry UDP (and even SOCKS5 UDP ASSOCIATE is
// rarely usable through corporate egress), so matched UDP flows are refused
// immediately (return false) instead of hanging — QUIC's UDP/443 fallback
// then fails fast and clients fall back to TCP.
func (u *Userspace) EnableNATWithRules(logger *log.Logger, rules []*ProxyRule, m *proxyMatcher, sink chan<- []byte) error {
	if m == nil {
		var err error
		if m, err = newProxyMatcher(rules); err != nil {
			return err
		}
	}
	logf := func(format string, args ...any) {}
	if logger != nil {
		logf = logger.Printf
	}

	tcpFwd := tcp.NewForwarder(u.stack, 0, 1024, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		dstAddrIP := id.LocalAddress
		dst := netipAddrFromTcpip(dstAddrIP)

		rule := m.Match(dst)
		if rule == nil {
			// Not covered by a proxy rule (shouldn't happen — WritePacket
			// filters — but refuse rather than leak).
			r.Complete(true)
			return
		}
		target := net.JoinHostPort(dst.String(), strconv.Itoa(int(id.LocalPort)))

		if !rule.SNI {
			// CONNECT first: fail fast (RST via Complete(true)) on proxy
			// error, and server-first protocols (SSH banner) can't deadlock.
			remote, err := dialProxy(rule.Proxy, target)
			if err != nil {
				logf("proxy: tcp %s: %v", target, err)
				r.Complete(true)
				return
			}
			var wq waiter.Queue
			ep, terr := r.CreateEndpoint(&wq)
			if terr != nil {
				remote.Close()
				r.Complete(true)
				return
			}
			r.Complete(false)
			go bridge(gonet.NewTCPConn(&wq, ep), remote)
			return
		}

		// SNI mode: accept locally first so the client can send its
		// ClientHello, peek it, then CONNECT with the hostname.
		var wq waiter.Queue
		ep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			r.Complete(true)
			return
		}
		r.Complete(false)
		local := gonet.NewTCPConn(&wq, ep)

		go func() {
			defer local.Close()
			peeked, host := peekSNI(local, dst)
			targetHost := host
			if targetHost == "" {
				targetHost = dst.String() // fallback: IP literal CONNECT
			}
			remote, err := dialProxy(rule.Proxy,
				net.JoinHostPort(targetHost, strconv.Itoa(int(id.LocalPort))))
			if err != nil {
				logf("proxy: tcp %s (sni=%s): %v", target, targetHost, err)
				return
			}
			defer remote.Close()
			if len(peeked) > 0 {
				if _, err := remote.Write(peeked); err != nil {
					return
				}
			}
			bridge(local, remote)
		}()
	})
	u.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	udpFwd := udp.NewForwarder(u.stack, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		dst := netipAddrFromTcpip(id.LocalAddress)
		if m.Match(dst) != nil {
			logf("proxy: udp %s:%d refused (proxy rules carry TCP only)",
				dst, id.LocalPort)
			return false
		}
		// Unmatched UDP in a split config: nothing sensible to do with it
		// here (the kernel datapath never sees it), so refuse as well.
		return false
	})
	u.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	u.startEgressLoopPkt(sink)
	logf("proxy: %d proxy rule(s) installed (TCP via proxy, UDP refused)", len(rules))
	return nil
}

// startEgressLoop bridges the netstack's NIC egress back to its inbound
// path — but only for packets where one end is a local address. Local
// sockets (gonet dials) emit their SYNs and receive their SYN-ACKs through
// the NIC; re-injecting those lets the promiscuous NIC deliver them to the
// forwarder or the socket. Transit packets between the tunnel peer and the
// proxied CIDRs (neither end local) must NOT be re-injected — the stack
// would forward them back out (HandleLocal=false) and loop forever; they
// are only forwarded to the WebSocket via the sink.
func (u *Userspace) startEgressLoop() {
	u.startEgressLoopPkt(nil)
}

// startEgressLoopPkt is startEgressLoop with an optional sink that receives
// every egress packet (the SplitInterface forwards them to the WebSocket).
func (u *Userspace) startEgressLoopPkt(sink chan<- []byte) {
	u.loopOnce.Do(func() {
		go func() {
			buf := make([]byte, MTU)
			for {
				n, err := u.ReadPacket(buf)
				if err != nil {
					return
				}
				pkt := make([]byte, n)
				copy(pkt, buf[:n])
				if u.hasLocalEndpoint(pkt) {
					_, _ = u.WritePacket(pkt)
				}
				if sink != nil {
					select {
					case sink <- pkt:
					default:
					}
				}
			}
		}()
	})
}

// hasLocalEndpoint reports whether the packet's source or destination IP is
// one of the tunnel's own addresses: traffic between a local socket and the
// netstack's forwarder, as opposed to transit between the tunnel peer and a
// proxied CIDR.
func (u *Userspace) hasLocalEndpoint(pkt []byte) bool {
	if len(pkt) == 0 {
		return false
	}
	var src, dst netip.Addr
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < 20 {
			return false
		}
		src = netip.AddrFrom4([4]byte(pkt[12:16]))
		dst = netip.AddrFrom4([4]byte(pkt[16:20]))
	case 6:
		if len(pkt) < 40 {
			return false
		}
		src = netip.AddrFrom16([16]byte(pkt[8:24]))
		dst = netip.AddrFrom16([16]byte(pkt[24:40]))
	default:
		return false
	}
	_, s := u.localIPs[src.Unmap()]
	_, d := u.localIPs[dst.Unmap()]
	return s || d
}

// peekSNI reads the client's first bytes with a short deadline and extracts
// the TLS SNI. Non-TLS, silent (SSH), oversized or malformed handshakes
// return (nil, "") — the flow falls back to an IP-literal CONNECT.
func peekSNI(conn *gonet.TCPConn, dst netip.Addr) ([]byte, string) {
	buf := make([]byte, 5+sniMaxRecord)
	_ = conn.SetReadDeadline(time.Now().Add(sniPeekTimeout))
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		// Enough for a full first record (or a parseable prefix)?
		if total >= 5 {
			recLen := int(buf[3])<<8 | int(buf[4])
			if recLen >= 1 && recLen <= sniMaxRecord && total >= 5+recLen {
				break
			}
		}
		if err != nil {
			break // timeout / close: parse whatever we have
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	if total == 0 {
		return nil, ""
	}
	if host := extractSNI(buf[:total]); host != "" {
		return buf[:total], host
	}
	return buf[:total], ""
}

func netipAddrFromTcpip(a tcpip.Address) netip.Addr {
	if a.Len() == 4 {
		return netip.AddrFrom4(a.As4())
	}
	return netip.AddrFrom16(a.As16())
}
