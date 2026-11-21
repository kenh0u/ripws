package tunnel

import (
	"encoding/binary"
	"net/netip"
)

// sniMaxRecord is the TLS record payload cap (RFC 5246: must not exceed
// 2^14 bytes). ClientHellos larger than this are malformed → fallback.
const sniMaxRecord = 16384

// extractSNI parses the TLS ClientHello in data and returns the server_name
// extension value, or "" when the data is not TLS, not a ClientHello, carries
// no (valid) SNI, or is malformed in any way. It is deliberately strict and
// allocation-light: an unparseable handshake must never error the flow, the
// caller simply falls back to the destination IP literal.
//
// The returned name, when non-empty, is already sanitized (see sanitizeSNI):
// 1..253 chars from [A-Za-z0-9.-] only, and not an IP literal. This makes it
// safe to splice into a "CONNECT host:port" request line without header
// injection risk.
func extractSNI(data []byte) string {
	host, ok := sniFromRecords(data)
	if !ok {
		return ""
	}
	return sanitizeSNI(host)
}

// sniMaxHandshake caps the total handshake message size we reassemble
// across fragmented records (2^14 + record headers).
const sniMaxHandshake = 4*sniMaxRecord + 16

// sniFromRecords walks TLS records looking for a handshake record whose
// payload begins with a ClientHello, reassembling a handshake message that
// is fragmented across multiple records (legal per RFC 5246 §6.2.1), and
// extracts the raw server_name.
func sniFromRecords(data []byte) (string, bool) {
	if len(data) < 5 || data[0] != 0x16 {
		return "", false // not a handshake record → not (yet) TLS
	}
	recLen := int(binary.BigEndian.Uint16(data[3:5]))
	if recLen < 4 || recLen > sniMaxRecord || len(data) < 5+recLen {
		return "", false
	}
	payload := data[5 : 5+recLen]
	if payload[0] != 0x01 { // first handshake message must be the ClientHello
		return "", false
	}
	hsLen := int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if hsLen < 34 || hsLen > sniMaxHandshake {
		return "", false // degenerate or absurd handshake length
	}
	body := payload[4:]
	// Reassemble a ClientHello fragmented across subsequent handshake records
	// (RFC 5246 §6.2.1). Peers rarely fragment, but a 500 ms peek window
	// must survive one that does.
	if len(body) < hsLen {
		total := len(body)
		data = data[5+recLen:]
		for len(data) >= 5 && data[0] == 0x16 && total < hsLen {
			rl := int(binary.BigEndian.Uint16(data[3:5]))
			if rl < 1 || rl > sniMaxRecord || len(data) < 5+rl {
				break
			}
			body = append(body, data[5:5+rl]...)
			total += rl
			data = data[5+rl:]
		}
		if total < hsLen {
			return "", false // never completed within the peeked bytes
		}
	}
	return sniFromHandshakeBody(body[:hsLen])
}

// sniFromHandshakeBody parses one complete ClientHello body (everything
// after the 4-byte handshake header).
func sniFromHandshakeBody(b []byte) (string, bool) {
	if len(b) < 2+32+1 { // version + random + session_id_len
		return "", false
	}
	b = b[2+32:] // skip version, random

	// session_id
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return "", false
	}
	b = b[1+int(b[0]):]

	// cipher_suites
	if len(b) < 2 || len(b) < 2+int(binary.BigEndian.Uint16(b)) {
		return "", false
	}
	b = b[2+int(binary.BigEndian.Uint16(b)):]

	// compression_methods
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return "", false
	}
	b = b[1+int(b[0]):]

	// extensions
	if len(b) < 2 {
		return "", false // ClientHello without extensions: no SNI
	}
	extLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if extLen > len(b) {
		return "", false
	}
	b = b[:extLen]

	for len(b) >= 4 {
		extType := binary.BigEndian.Uint16(b)
		l := int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+l {
			return "", false
		}
		body := b[4 : 4+l]
		if extType == 0x0000 { // server_name
			return sniFromExtension(body)
		}
		b = b[4+l:]
	}
	return "", false
}

// sniFromHandshake parses one handshake payload (ClientHello only).
func sniFromHandshake(b []byte) (string, bool) {
	if len(b) < 4 || b[0] != 0x01 { // handshake type: ClientHello
		return "", false
	}
	hsLen := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if hsLen < 4 {
		return "", false // body shorter than the fixed ClientHello fields
	}
	b = b[4:]            // full handshake body; trailing bytes past hsLen are ignored
	if len(b) < 2+32+1 { // version + random + session_id_len
		return "", false
	}
	b = b[2+32:] // skip version, random

	// session_id
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return "", false
	}
	b = b[1+int(b[0]):]

	// cipher_suites
	if len(b) < 2 || len(b) < 2+int(binary.BigEndian.Uint16(b)) {
		return "", false
	}
	b = b[2+int(binary.BigEndian.Uint16(b)):]

	// compression_methods
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return "", false
	}
	b = b[1+int(b[0]):]

	// extensions
	if len(b) < 2 {
		return "", false // ClientHello without extensions: no SNI
	}
	extLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if extLen > len(b) {
		return "", false
	}
	b = b[:extLen]

	for len(b) >= 4 {
		extType := binary.BigEndian.Uint16(b)
		l := int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+l {
			return "", false
		}
		body := b[4 : 4+l]
		if extType == 0x0000 { // server_name
			return sniFromExtension(body)
		}
		b = b[4+l:]
	}
	return "", false
}

// sniFromExtension parses the server_name extension body.
func sniFromExtension(b []byte) (string, bool) {
	if len(b) < 2 {
		return "", false
	}
	listLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if listLen > len(b) {
		return "", false
	}
	b = b[:listLen]
	if len(b) < 4 {
		return "", false
	}
	if b[0] != 0x00 { // host_name entry type
		return "", false
	}
	nameLen := int(binary.BigEndian.Uint16(b[1:3]))
	if len(b) < 3+nameLen {
		return "", false
	}
	return string(b[3 : 3+nameLen]), true
}

// sanitizeSNI enforces the hostname shape before the value is used as a
// proxy CONNECT target: length 1..253, ASCII letters/digits/dots/hyphens
// only. Anything else — including embedded CR/LF/space/colon (header
// injection) and IP literals (pointless as a domain) — yields "".
func sanitizeSNI(s string) string {
	if len(s) < 1 || len(s) > 253 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-':
		default:
			return ""
		}
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return "" // an IP literal is not a domain; use dstIP directly
	}
	return s
}
