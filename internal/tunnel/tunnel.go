// Package tunnel abstracts the L3 ingress/egress interface used by RIPWS.
//
// A tunnel carries raw IP packets between the RIPWS data plane and either the
// host kernel (via a TUN device) or a userspace network stack (gVisor
// netstack). The Interface contract is deliberately minimal so the pipe layer
// can treat both backends identically, mirroring Tailscale's
// userspace-networking separation between wire transport and L3 handling.
package tunnel

// Interface is a bidirectional raw-IP packet conduit.
//
// Implementations must be safe for a single reader and a single writer
// (operating concurrently); they are not required to support concurrent
// readers or concurrent writers.
type Interface interface {
	// ReadPacket reads a single inbound IP packet (as seen by the local
	// network stack) into p and returns its length. It blocks until a packet
	// is available or the interface is closed.
	ReadPacket(p []byte) (n int, err error)

	// WritePacket writes a single outbound IP packet (received from the
	// remote peer over the WebSocket) into the local network stack.
	WritePacket(p []byte) (n int, err error)

	// Name returns the interface name, e.g. "tun0" for kernel mode or a
	// synthetic label for userspace mode.
	Name() string

	// Close releases all resources. It is idempotent.
	Close() error
}

// MTU is the default RIPWS tunnel MTU. WebSocket binary frames have no
// inherent size limit and the outer TCP/TLS path MTU is handled by the kernel,
// so we pick the conventional TUN MTU that fits within Ethernet while leaving
// headroom for the outer encapsulation on the wire.
const MTU = 1500
