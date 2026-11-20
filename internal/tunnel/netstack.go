//go:build linux

// Package tunnel - userspace netstack backend backed by gVisor.
//
// This backend runs a full L3 stack in user space: no /dev/net/tun and no
// CAP_NET_ADMIN are required. Raw IP packets arriving over the WebSocket are
// injected into a gVisor stack via a channel link endpoint; packets the stack
// emits (in response, or from a userspace socket) are read back out and
// forwarded over the WebSocket.
//
// This mirrors Tailscale's "userspace networking" mode, where the virtual NIC
// is backed by gVisor's netstack so applications dialing through the device
// get kernel-free L3 routing.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// Route describes a routing-table entry installed in the userspace stack.
type Route struct {
	// Destination in CIDR notation, e.g. "10.8.0.0/24". A default route is
	// "0.0.0.0/0" (v4) or "::/0" (v6).
	Destination string
	// Gateway is optional; if empty the route is directly connected via the
	// tunnel NIC.
	Gateway string
}

// Userspace is a tunnel.Interface backed by an in-process gVisor netstack.
type Userspace struct {
	stack    *stack.Stack
	link     *channel.Endpoint
	nicID    tcpip.NICID
	mu       sync.Mutex
	closed   bool
	loopOnce sync.Once
}

// NewUserspace creates a netstack with the given local addresses and routes.
// localAddrs are CIDR-less addresses (e.g. "10.8.0.1") assigned to the tunnel
// NIC; routes are installed on creation.
func NewUserspace(localAddrs []string, routes []Route) (*Userspace, error) {
	return NewUserspaceMTU(localAddrs, routes, MTU)
}

// NewUserspaceMTU is NewUserspace with an explicit tunnel MTU (0 = default).
// The MTU must match the configured tunnel MTU so the netstack and the
// WebSocket frame limit agree.
func NewUserspaceMTU(localAddrs []string, routes []Route, mtu int) (*Userspace, error) {
	if mtu <= 0 {
		mtu = MTU
	}
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
			icmp.NewProtocol6,
		},
		HandleLocal: false,
	})

	linkAddr := tcpip.LinkAddress("\x02\x00\x00\x00\x00\x01")
	ch := channel.New(512, uint32(mtu), linkAddr)

	nicID := tcpip.NICID(1)
	if err := s.CreateNIC(nicID, ch); err != nil {
		return nil, fmt.Errorf("ripws: create NIC: %v", err)
	}

	for _, a := range localAddrs {
		proto, addr, err := parseAddr(a)
		if err != nil {
			return nil, err
		}
		pa := tcpip.ProtocolAddress{Protocol: proto, AddressWithPrefix: addr.WithPrefix()}
		if err := s.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
			return nil, fmt.Errorf("ripws: add address %s: %v", a, err)
		}
	}

	// Spoofing lets the stack accept any source; promiscuity lets it receive
	// packets not addressed to a local IP. IP forwarding lets transit packets
	// (src/dst both off-link, e.g. NAT gateway flows) be re-emitted on the
	// link instead of dropped by the forwarding check.
	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)
	for _, p := range []tcpip.NetworkProtocolNumber{ipv4.ProtocolNumber, ipv6.ProtocolNumber} {
		if err := s.SetForwardingDefaultAndAllNICs(p, true); err != nil {
			return nil, fmt.Errorf("ripws: enable forwarding for %v: %v", p, err)
		}
	}

	rt, err := buildRouteTable(nicID, routes)
	if err != nil {
		return nil, err
	}
	s.SetRouteTable(rt)

	return &Userspace{stack: s, link: ch, nicID: nicID}, nil
}

// ReadPacket returns the next packet the netstack wants to emit on the tunnel
// (i.e. outbound from the local stack towards the remote peer). It blocks
// until a packet is available or Close is called.
func (u *Userspace) ReadPacket(p []byte) (int, error) {
	return u.ReadPacketCtx(context.Background(), p)
}

// ReadPacketCtx is ReadPacket with cancellation.
func (u *Userspace) ReadPacketCtx(ctx context.Context, p []byte) (int, error) {
	pkt := u.link.ReadContext(ctx)
	if pkt == nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return 0, net.ErrClosed
	}
	defer pkt.DecRef()

	view := pkt.ToView()
	defer view.Release()
	data := view.AsSlice()
	n := copy(p, data)
	return n, nil
}

// WritePacket injects a raw IP packet (received from the remote peer over the
// WebSocket) into the local netstack for routing/processing.
func (u *Userspace) WritePacket(p []byte) (int, error) {
	if len(p) < 1 {
		return 0, errors.New("ripws: empty packet")
	}
	var proto tcpip.NetworkProtocolNumber
	switch p[0] >> 4 {
	case 4:
		proto = ipv4.ProtocolNumber
	case 6:
		proto = ipv6.ProtocolNumber
	default:
		return 0, errors.New("ripws: unknown IP version")
	}

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		// MakeWithData copies p; nothing retains the caller's slice.
		Payload: buffer.MakeWithData(p),
	})
	defer pkt.DecRef()
	u.link.InjectInbound(proto, pkt)
	return len(p), nil
}

// Name returns a synthetic interface name.
func (u *Userspace) Name() string { return "ripws-netstack" }

// Close tears down the stack. Idempotent.
func (u *Userspace) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	u.closed = true
	u.link.Close()
	u.stack.Close()
	return nil
}

// Stack returns the underlying gVisor stack, enabling callers to attach
// userspace sockets (via gonet) for end-to-end userspace tests.
func (u *Userspace) Stack() *stack.Stack { return u.stack }

// NICID returns the tunnel NIC's identifier.
func (u *Userspace) NICID() tcpip.NICID { return u.nicID }

func parseAddr(s string) (tcpip.NetworkProtocolNumber, tcpip.Address, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, tcpip.Address{}, fmt.Errorf("ripws: bad IP %q", s)
	}
	if v4 := ip.To4(); v4 != nil {
		return ipv4.ProtocolNumber, tcpip.AddrFrom4([4]byte(v4)), nil
	}
	return ipv6.ProtocolNumber, tcpip.AddrFrom16([16]byte(ip)), nil
}

func buildRouteTable(nicID tcpip.NICID, routes []Route) ([]tcpip.Route, error) {
	out := make([]tcpip.Route, 0, len(routes))
	for _, r := range routes {
		subnet, err := parseSubnet(r.Destination)
		if err != nil {
			return nil, err
		}
		rt := tcpip.Route{Destination: subnet, NIC: nicID}
		if r.Gateway != "" {
			_, gw, err := parseAddr(r.Gateway)
			if err != nil {
				return nil, err
			}
			rt.Gateway = gw
		}
		out = append(out, rt)
	}
	return out, nil
}

func parseSubnet(s string) (tcpip.Subnet, error) {
	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return tcpip.Subnet{}, err
	}
	ones, _ := ipnet.Mask.Size()
	var addr tcpip.Address
	if v4 := ipnet.IP.To4(); v4 != nil {
		addr = tcpip.AddrFrom4([4]byte(v4))
	} else {
		addr = tcpip.AddrFrom16([16]byte(ipnet.IP))
	}
	return tcpip.AddressWithPrefix{Address: addr, PrefixLen: ones}.Subnet(), nil
}

var _ Interface = (*Userspace)(nil)
