package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

// openBSDPIHeaderLen is the mandatory 4-byte network-byte-order address-family
// header that OpenBSD's Layer-3 tun(4) driver prepends to every packet read
// from or written to /dev/tunN.
const openBSDPIHeaderLen = 4

// prependOpenBSDPIHeader prefixes pkt with the 4-byte big-endian address family
// header required when writing L3 packets into OpenBSD's /dev/tunN.
func prependOpenBSDPIHeader(pkt []byte, af4, af6 uint32) ([]byte, error) {
	if len(pkt) == 0 {
		return nil, errors.New("ripws: empty packet")
	}
	var af uint32
	switch ver := pkt[0] >> 4; ver {
	case 4:
		af = af4
	case 6:
		af = af6
	default:
		return nil, fmt.Errorf("ripws: unknown IP version %d", ver)
	}
	out := make([]byte, openBSDPIHeaderLen+len(pkt))
	binary.BigEndian.PutUint32(out[:openBSDPIHeaderLen], af)
	copy(out[openBSDPIHeaderLen:], pkt)
	return out, nil
}

// stripOpenBSDPIHeader validates and strips the 4-byte big-endian address
// family header read from OpenBSD's /dev/tunN, returning the raw IP payload.
func stripOpenBSDPIHeader(frame []byte, af4, af6 uint32) ([]byte, error) {
	if len(frame) <= openBSDPIHeaderLen {
		return nil, fmt.Errorf("ripws: short openbsd tun frame (%d bytes)", len(frame))
	}
	af := binary.BigEndian.Uint32(frame[:openBSDPIHeaderLen])
	if af != af4 && af != af6 {
		return nil, fmt.Errorf("ripws: unsupported openbsd tun address family %d", af)
	}
	return frame[openBSDPIHeaderLen:], nil
}

// parseOpenBSDTunUnit checks whether name is an explicit OpenBSD tun interface
// name ("tun0", "tun1", ...) and returns its unit number.
func parseOpenBSDTunUnit(name string) (int, bool) {
	const prefix = "tun"
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
		return 0, false
	}
	suffix := name[len(prefix):]
	if len(suffix) > 1 && suffix[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return 0, false
		}
	}
	unit, err := strconv.Atoi(suffix)
	if err != nil || unit < 0 {
		return 0, false
	}
	return unit, true
}
