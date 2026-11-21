package tunnel

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestOpenBSDPIHeaderRoundTrip(t *testing.T) {
	const (
		af4 = 2
		af6 = 24
	)
	v4 := []byte{0x45, 0x00, 0x00, 0x14, 1, 2, 3, 4}
	framed4, err := prependOpenBSDPIHeader(v4, af4, af6)
	if err != nil {
		t.Fatalf("prepend v4: %v", err)
	}
	if len(framed4) != 4+len(v4) || binary.BigEndian.Uint32(framed4[:4]) != af4 {
		t.Fatalf("unexpected v4 header: %x", framed4[:4])
	}
	got4, err := stripOpenBSDPIHeader(framed4, af4, af6)
	if err != nil || !bytes.Equal(got4, v4) {
		t.Fatalf("strip v4 = (%x, %v), want %x", got4, err, v4)
	}

	v6 := []byte{0x60, 0x00, 0x00, 0x00, 0xaa, 0xbb}
	framed6, err := prependOpenBSDPIHeader(v6, af4, af6)
	if err != nil {
		t.Fatalf("prepend v6: %v", err)
	}
	if binary.BigEndian.Uint32(framed6[:4]) != af6 {
		t.Fatalf("unexpected v6 header: %x", framed6[:4])
	}
	got6, err := stripOpenBSDPIHeader(framed6, af4, af6)
	if err != nil || !bytes.Equal(got6, v6) {
		t.Fatalf("strip v6 = (%x, %v), want %x", got6, err, v6)
	}
}

func TestOpenBSDPIHeaderErrors(t *testing.T) {
	if _, err := prependOpenBSDPIHeader(nil, 2, 24); err == nil {
		t.Error("expected error on empty packet")
	}
	if _, err := prependOpenBSDPIHeader([]byte{0x35, 0x00}, 2, 24); err == nil {
		t.Error("expected error on non-IPv4/IPv6 version")
	}
	for _, bad := range [][]byte{
		{},
		{0, 0, 0, 2},
		{0, 0, 0, 99, 0x45},
	} {
		if _, err := stripOpenBSDPIHeader(bad, 2, 24); err == nil {
			t.Errorf("expected error for frame %x", bad)
		}
	}
}

func TestParseOpenBSDTunUnit(t *testing.T) {
	good := map[string]int{
		"tun0":  0,
		"tun1":  1,
		"tun12": 12,
	}
	for name, want := range good {
		got, ok := parseOpenBSDTunUnit(name)
		if !ok || got != want {
			t.Errorf("parseOpenBSDTunUnit(%q) = (%d, %v), want (%d, true)", name, got, ok, want)
		}
	}
	for _, bad := range []string{"", "tun", "tun00", "tun01", "tun-1", "tunn", "ws0", "ripws0"} {
		if got, ok := parseOpenBSDTunUnit(bad); ok {
			t.Errorf("parseOpenBSDTunUnit(%q) = (%d, true), want false", bad, got)
		}
	}
}
