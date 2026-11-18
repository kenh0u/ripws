//go:build linux

package tunnel

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

var (
	udpProto  = udp.ProtocolNumber
	ipv4Proto = ipv4.ProtocolNumber
)

func tcpipFullAddress(port uint16) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Port: port}
}

// buildUDP4 constructs a minimal UDP-over-IPv4 packet with a valid checksum.
func buildUDP4(t *testing.T, src, dst string, sport, dport uint16, payload []byte) []byte {
	t.Helper()

	srcIP := net.ParseIP(src).To4()
	dstIP := net.ParseIP(dst).To4()
	if srcIP == nil || dstIP == nil {
		t.Fatal("need IPv4 addrs")
	}

	total := 20 + 8 + len(payload)
	const proto = 17 // UDP
	pkt := make([]byte, total)
	pkt[0] = 0x45
	pkt[2] = byte(total >> 8)
	pkt[3] = byte(total)
	pkt[8] = 64 // TTL
	pkt[9] = 17 // UDP
	copy(pkt[12:16], srcIP)
	copy(pkt[16:20], dstIP)

	binary.BigEndian.PutUint16(pkt[20:], sport)
	binary.BigEndian.PutUint16(pkt[22:], dport)
	binary.BigEndian.PutUint16(pkt[24:], uint16(8+len(payload)))
	copy(pkt[28:], payload)

	// IPv4 header checksum.
	ipSum := uint32(0)
	for i := 0; i < 20; i += 2 {
		ipSum += uint32(pkt[i])<<8 | uint32(pkt[i+1])
	}
	for ipSum>>16 != 0 {
		ipSum = (ipSum >> 16) + (ipSum & 0xffff)
	}
	binary.BigEndian.PutUint16(pkt[10:], ^uint16(ipSum))

	// UDP checksum over pseudo-header (src, dst, zero, proto, UDP length)
	// plus the UDP datagram itself (checksum field zero).
	sum := uint32(0)
	for i := 0; i < 4; i += 2 {
		sum += uint32(srcIP[i])<<8 | uint32(srcIP[i+1])
		sum += uint32(dstIP[i])<<8 | uint32(dstIP[i+1])
	}
	sum += uint32(proto) + uint32(8+len(payload))
	for i := 20; i < total; i += 2 {
		if i+1 < total {
			sum += uint32(pkt[i])<<8 | uint32(pkt[i+1])
		} else {
			sum += uint32(pkt[i]) << 8
		}
	}
	for sum>>16 != 0 {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	binary.BigEndian.PutUint16(pkt[26:], ^uint16(sum))
	return pkt
}

// TestUserspaceUDPDelivery verifies the WS->tun path end to end: a UDP
// packet injected from the "remote peer" reaches a local gonet socket bound
// inside the netstack, and its reply is emitted back out the link.
func TestUserspaceUDPDelivery(t *testing.T) {
	u, err := NewUserspaceMTU([]string{"10.7.0.2"}, []Route{{Destination: "0.0.0.0/0"}}, 1420)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	// Local UDP socket inside the netstack on 10.7.0.2:5200.
	var wq waiter.Queue
	ep, terr := u.stack.NewEndpoint(udpProto, ipv4Proto, &wq)
	if terr != nil {
		t.Fatalf("NewEndpoint: %v", terr)
	}
	defer ep.Close()
	if err := ep.Bind(tcpipFullAddress(5200)); err != nil {
		t.Fatalf("bind: %v", err)
	}
	conn := gonet.NewUDPConn(&wq, ep)
	defer conn.Close()

	// Remote peer 10.7.0.1:5300 -> 10.7.0.2:5200.
	pkt := buildUDP4(t, "10.7.0.1", "10.7.0.2", 5300, 5200, []byte("ping"))
	if _, err := u.WritePacket(pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	buf := make([]byte, 64)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, raddr, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("local socket read: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("payload = %q", buf[:n])
	}
	if raddr.String() != "10.7.0.1:5300" {
		t.Fatalf("remote addr = %s", raddr)
	}

	// Reply: the socket's response must come back out the link.
	if _, err := conn.WriteTo([]byte("pong"), raddr); err != nil {
		t.Fatalf("write reply: %v", err)
	}
	out := make([]byte, MTU)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	on, err := u.ReadPacketCtx(ctx, out)
	if err != nil {
		t.Fatalf("ReadPacketCtx (reply not emitted): %v", err)
	}
	// src 10.7.0.2:5200 -> dst 10.7.0.1:5300, payload "pong"
	if on < 32 || string(out[28:on]) != "pong" {
		t.Fatalf("emitted reply = %x", out[:on])
	}
	if binary.BigEndian.Uint16(out[20:22]) != 5200 || binary.BigEndian.Uint16(out[22:24]) != 5300 {
		t.Fatalf("reply ports wrong: %x", out[20:24])
	}
}

// TestUserspaceReadCancel verifies ReadPacketCtx unblocks on cancellation —
// the property that fixes the pipe shutdown deadlock.
func TestUserspaceReadCancel(t *testing.T) {
	u, err := NewUserspace([]string{"10.7.0.2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = u.ReadPacketCtx(ctx, make([]byte, MTU))
	if err == nil {
		t.Fatal("expected ctx error from cancelled read")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancellation took %v; read did not unblock promptly", d)
	}
}
