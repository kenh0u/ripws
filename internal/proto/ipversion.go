package proto

import "errors"

// IP version nibble values extracted from the first octet of a raw IP packet.
const (
	IPv4Version = 0x4
	IPv6Version = 0x6
)

// MinIPHeaderLen is the smallest legal IP packet we are willing to forward.
// Anything shorter cannot contain even a minimal IPv4 header and is dropped.
const MinIPHeaderLen = 20

// ErrShortPacket is returned when a frame is too small to contain an IP header.
var ErrShortPacket = errors.New("ripws: packet shorter than minimum IP header")

// ErrUnknownIPVersion is returned when the version nibble is neither 4 nor 6.
var ErrUnknownIPVersion = errors.New("ripws: unknown IP version nibble")

// IPVersion inspects the leading 4 bits of a raw IP packet and returns the
// version (4 or 6). It performs only the minimum validation required to route
// the packet; full header sanity checking is delegated to the kernel or the
// userspace network stack that ultimately consumes the payload.
func IPVersion(p []byte) (int, error) {
	if len(p) < 1 {
		return 0, ErrShortPacket
	}
	switch p[0] >> 4 {
	case IPv4Version:
		if len(p) < MinIPHeaderLen {
			return 0, ErrShortPacket
		}
		return IPv4Version, nil
	case IPv6Version:
		if len(p) < 40 {
			return 0, ErrShortPacket
		}
		return IPv6Version, nil
	default:
		return 0, ErrUnknownIPVersion
	}
}
