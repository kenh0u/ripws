//go:build linux

package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// PingResult is a single ICMP echo probe result.
type PingResult struct {
	Seq   int
	RTT   time.Duration
	Error error
}

// Ping sends count ICMP echo requests through the userspace netstack to dst
// and returns results on the returned channel (closed when all probes complete
// or ctx is cancelled). interval governs the spacing between probes.
func (u *Userspace) Ping(ctx context.Context, dst string, count int, interval time.Duration) <-chan PingResult {
	ch := make(chan PingResult, count)
	go u.runPing(ctx, dst, count, interval, ch)
	return ch
}

func (u *Userspace) runPing(ctx context.Context, dst string, count int, interval time.Duration, ch chan<- PingResult) {
	defer close(ch)

	proto, dstAddr, err := parseAddr(dst)
	if err != nil {
		for i := 0; i < count; i++ {
			ch <- PingResult{Seq: i, Error: err}
		}
		return
	}
	if proto != ipv4.ProtocolNumber {
		for i := 0; i < count; i++ {
			ch <- PingResult{Seq: i, Error: fmt.Errorf("userspace ping: only IPv4 supported")}
		}
		return
	}

	var wq waiter.Queue
	ep, terr := u.stack.NewEndpoint(icmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		for i := 0; i < count; i++ {
			ch <- PingResult{Seq: i, Error: fmt.Errorf("userspace ping: new endpoint: %v", terr)}
		}
		return
	}
	defer ep.Close()

	if terr := ep.Bind(tcpip.FullAddress{NIC: u.nicID}); terr != nil {
		for i := 0; i < count; i++ {
			ch <- PingResult{Seq: i, Error: fmt.Errorf("userspace ping: bind: %v", terr)}
		}
		return
	}

	if terr := ep.Connect(tcpip.FullAddress{Addr: dstAddr, NIC: u.nicID}); terr != nil {
		for i := 0; i < count; i++ {
			ch <- PingResult{Seq: i, Error: fmt.Errorf("userspace ping: connect: %v", terr)}
		}
		return
	}

	// Register a channel-based waiter for readability events.
	notifyCh := make(chan struct{}, 1)
	entry := waiter.NewFunctionEntry(waiter.EventIn, func(waiter.EventMask) {
		select {
		case notifyCh <- struct{}{}:
		default:
		}
	})
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)

	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			ch <- PingResult{Seq: i, Error: err}
			return
		}

		pkt := buildICMPEcho(i)
		start := time.Now()

		_, werr := ep.Write(bytes.NewReader(pkt), tcpip.WriteOptions{})
		if werr != nil {
			ch <- PingResult{Seq: i, Error: fmt.Errorf("userspace ping: write: %v", werr)}
			if i < count-1 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(interval):
				}
			}
			continue
		}

		// Wait for response with timeout.
		gotReply := false
		deadline := time.Now().Add(5 * time.Second)
		for {
			// Try non-blocking read first.
			var buf bytes.Buffer
			result, rerr := ep.Read(&buf, tcpip.ReadOptions{})
			if rerr == nil {
				_ = result
				gotReply = true
				break
			}
			if rerr.String() != "operation would block" {
				ch <- PingResult{Seq: i, Error: fmt.Errorf("userspace ping: read: %v", rerr)}
				gotReply = true // error counts as "handled"
				break
			}

			// Wait for event or timeout.
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			select {
			case <-ctx.Done():
				ch <- PingResult{Seq: i, Error: ctx.Err()}
				return
			case <-notifyCh:
				// Drain any extra notifications.
				select {
				case <-notifyCh:
				default:
				}
				// Loop back to try reading again.
			case <-time.After(remaining):
				break
			}
			if time.Now().After(deadline) {
				break
			}
		}

		if gotReply {
			rtt := time.Since(start)
			ch <- PingResult{Seq: i, RTT: rtt}
		} else {
			ch <- PingResult{Seq: i, Error: fmt.Errorf("userspace ping: timeout")}
		}

		if i < count-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}
}

// buildICMPEcho constructs an ICMPv4 Echo Request message body.
func buildICMPEcho(seq int) []byte {
	msg := make([]byte, 8+56) // 8-byte header + 56-byte payload
	msg[0] = byte(header.ICMPv4Echo)
	msg[1] = 0
	binary.BigEndian.PutUint16(msg[6:], uint16(seq))
	for i := 8; i < len(msg); i++ {
		msg[i] = byte(i)
	}
	cs := icmpChecksum(msg)
	binary.BigEndian.PutUint16(msg[2:], cs)
	return msg
}

func icmpChecksum(b []byte) uint16 {
	sum := 0
	for i := 0; i < len(b)-1; i += 2 {
		sum += int(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += int(b[len(b)-1]) << 8
	}
	for sum > 0xFFFF {
		sum = (sum & 0xFFFF) + (sum >> 16)
	}
	return ^uint16(sum)
}

var _ = net.IPv4
