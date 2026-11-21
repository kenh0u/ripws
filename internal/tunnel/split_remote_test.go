package tunnel

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// TestSplitRemoteInitiatedFlow drives the split dataplane the way the VM
// does: the tunnel peer (server) initiates a TCP connection to a proxied
// CIDR by having raw packets injected via WritePacket (the WS path), not by
// a local gonet dial.
func TestSplitRemoteInitiatedFlow(t *testing.T) {
	proxy := startMockProxy(t, func(string) bool { return true }, "echo")
	primary := newPacketPipe()
	rules := []*ProxyRule{{
		CIDRs: []string{"198.51.100.0/24"},
		Proxy: "http://" + proxy.addr(),
		SNI:   false,
	}}
	split, err := NewSplit(primary, []string{"10.12.0.2"}, rules, 1500, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer split.Close()

	// Use the kernel-style approach: a raw TCP SYN from 10.12.0.1:40000 to
	// 198.51.100.10:443 arrives over the "WebSocket".
	a := relay(t, split, "10.12.0.1")

	// Server-side dial to the proxied IP: raw TCP through the tunnel.
	dctx, dcancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer dcancel()
	c, err := gonet.DialTCPWithBind(dctx, a.stack,
		tcpipFullAddr("10.12.0.1", 0), tcpipFullAddr("198.51.100.10", 443), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("tunneled-hello")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no echo through split proxy: %v", err)
	}
	if string(buf[:n]) != "tunneled-hello" {
		t.Fatalf("echo = %q", buf[:n])
	}
	if got := proxy.recorded(); len(got) == 0 || got[0] != "198.51.100.10:443" {
		t.Fatalf("targets = %v", got)
	}
	// The proxy must receive a CONNECT within a few seconds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := proxy.recorded(); len(got) > 0 {
			if got[0] != "198.51.100.10:443" {
				t.Fatalf("target = %v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy saw no CONNECT from remote-initiated flow")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const flagSYN = 0x02

// relay simulates the two WebSocket legs between the server stack A and
// the split interface, so remote-initiated flows can be driven end to end.
func relay(t *testing.T, split Interface, serverIP string) *Userspace {
	t.Helper()
	a, err := NewUserspaceMTU([]string{serverIP}, []Route{{Destination: "0.0.0.0/0"}}, 1500)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	var wg sync.WaitGroup
	wg.Add(2)
	pumpPkt := func(from, to Interface) {
		defer wg.Done()
		buf := make([]byte, MTU)
		for {
			n, err := from.ReadPacket(buf)
			if err != nil {
				return
			}
			if _, err := to.WritePacket(buf[:n]); err != nil {
				return
			}
		}
	}
	go pumpPkt(a, split) // server egress -> client WS ingress
	go pumpPkt(split, a) // client WS egress -> server ingress
	return a
}

// buildTCP4 forges a checksummed IPv4+TCP SYN packet.
var _ = buildTCP4

func buildTCP4(t *testing.T, src, dst string, sport, dport uint16) []byte {
	t.Helper()
	srcIP := net.ParseIP(src).To4()
	dstIP := net.ParseIP(dst).To4()
	if srcIP == nil || dstIP == nil {
		t.Fatal("need IPv4 addrs")
	}
	const proto = 6
	total := 40
	pkt := make([]byte, total)
	pkt[0] = 0x45
	pkt[2] = byte(total >> 8)
	pkt[3] = byte(total)
	pkt[4] = 0x12 // arbitrary IP ID
	pkt[6] = 0x40 // Don't Fragment
	pkt[8] = 64
	pkt[9] = proto
	copy(pkt[12:16], srcIP)
	copy(pkt[16:20], dstIP)

	binary.BigEndian.PutUint16(pkt[20:], sport)
	binary.BigEndian.PutUint16(pkt[22:], dport)
	binary.BigEndian.PutUint32(pkt[24:], 0x1000) // seq
	binary.BigEndian.PutUint32(pkt[28:], 0)      // ack
	pkt[32] = 0x50                               // data offset 5
	pkt[33] = flagSYN
	binary.BigEndian.PutUint16(pkt[34:], 0xffff) // window
	// TCP options: MSS
	mss := []byte{0x02, 0x04, 0x05, 0xb4}
	copy(pkt[40-len(mss):], mss) // no space; skip MSS

	// IPv4 header checksum.
	ipSum := uint32(0)
	for i := 0; i < 20; i += 2 {
		ipSum += uint32(pkt[i])<<8 | uint32(pkt[i+1])
	}
	for ipSum>>16 != 0 {
		ipSum = (ipSum >> 16) + (ipSum & 0xffff)
	}
	binary.BigEndian.PutUint16(pkt[10:], ^uint16(ipSum))

	// TCP checksum over pseudo-header + segment.
	sum := uint32(0)
	for i := 0; i < 4; i += 2 {
		sum += uint32(srcIP[i])<<8 | uint32(srcIP[i+1])
		sum += uint32(dstIP[i])<<8 | uint32(dstIP[i+1])
	}
	sum += proto + uint32(total-20)
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
	binary.BigEndian.PutUint16(pkt[36:], ^uint16(sum))
	return pkt
}

func tcpipFullAddr(ip string, port uint16) tcpip.FullAddress {
	_, a, err := parseAddr(ip)
	if err != nil {
		panic(err)
	}
	return tcpip.FullAddress{Addr: a, Port: port}
}
