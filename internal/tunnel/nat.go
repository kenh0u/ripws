//go:build linux

package tunnel

import (
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// EnableNAT installs TCP and UDP forwarders on the netstack so that transit
// traffic arriving from the tunnel (destined for external IPs) is transparently
// proxied out through the host's kernel networking via net.Dial.
//
// This is the userspace equivalent of iptables MASQUERADE + IP forwarding,
// requiring no CAP_NET_ADMIN or kernel TUN device. The gVisor netstack handles
// the full TCP/UDP state machine; we merely bridge each accepted connection to
// a real net.Dial connection on the host side.
//
// Flow:
//  1. Server sends TCP SYN to 8.8.8.8:443 via tunnel (arrives at netstack)
//  2. Netstack accepts it (spoofing); no local listener → forwarder fires
//  3. Forwarder handler: net.Dial("tcp", "8.8.8.8:443") via host kernel
//  4. Create netstack endpoint, bridge gonet.Conn ↔ net.Conn
//  5. Netstack sends SYN-ACK back through tunnel → server receives it
func (u *Userspace) EnableNAT(logger *log.Logger) error {
	// --- TCP forwarder ---
	tcpFwd := tcp.NewForwarder(u.stack, 0, 1024, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		dst := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))

		// Dial out via host kernel networking (no privileges needed).
		remoteConn, err := net.DialTimeout("tcp", dst, 10*time.Second)
		if err != nil {
			if logger != nil {
				logger.Printf("nat: tcp dial %s: %v", dst, err)
			}
			r.Complete(true) // send RST so caller knows connection refused
			return
		}

		var wq waiter.Queue
		ep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			if logger != nil {
				logger.Printf("nat: tcp create endpoint: %v", terr)
			}
			remoteConn.Close()
			r.Complete(true)
			return
		}

		localConn := gonet.NewTCPConn(&wq, ep)
		if logger != nil {
			logger.Printf("nat: tcp %s:%d -> %s", id.RemoteAddress, id.RemotePort, dst)
		}
		go func() {
			bridge(localConn, remoteConn)
			if logger != nil {
				logger.Printf("nat: tcp %s closed", dst)
			}
		}()
	})
	u.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	// --- UDP forwarder ---
	udpFwd := udp.NewForwarder(u.stack, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		dst := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))

		remoteConn, err := net.Dial("udp", dst)
		if err != nil {
			if logger != nil {
				logger.Printf("nat: udp dial %s: %v", dst, err)
			}
			return false
		}

		var wq waiter.Queue
		ep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			if logger != nil {
				logger.Printf("nat: udp create endpoint: %v", terr)
			}
			remoteConn.Close()
			return false
		}

		localConn := gonet.NewUDPConn(&wq, ep)
		if logger != nil {
			logger.Printf("nat: udp %s:%d -> %s", id.RemoteAddress, id.RemotePort, dst)
		}
		go func() {
			udpBridge(localConn, remoteConn)
			if logger != nil {
				logger.Printf("nat: udp %s closed", dst)
			}
		}()
		return true
	})
	u.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	if logger != nil {
		logger.Printf("nat: userspace NAT enabled (TCP+UDP forwarder installed)")
	}
	return nil
}

// udpBridge copies data bidirectionally between a netstack UDP conn and a
// kernel UDP conn. It uses ReadFrom/WriteTo on the kernel side to preserve the
// remote address. The netstack side is connection-oriented (connected to the
// original source).
func udpBridge(local *gonet.UDPConn, remote net.Conn) {
	defer local.Close()
	defer remote.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// netstack -> kernel
	go func() {
		defer wg.Done()
		buf := make([]byte, MTU)
		for {
			n, err := local.Read(buf)
			if err != nil {
				return
			}
			if _, err := remote.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	// kernel -> netstack
	go func() {
		defer wg.Done()
		buf := make([]byte, MTU)
		for {
			n, err := remote.Read(buf)
			if err != nil {
				return
			}
			if _, err := local.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	wg.Wait()
}

var _ = io.Copy
var _ = fmt.Sprintf
var _ = stack.TransportEndpointID{}
