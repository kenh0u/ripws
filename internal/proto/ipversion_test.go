package proto

import "testing"

func TestIPVersion(t *testing.T) {
	cases := []struct {
		name string
		pkt  []byte
		want int
		err  bool
	}{
		{"empty", nil, 0, true},
		{"short", []byte{0x45}, 0, true},
		{"ipv4", makeIPv4(), 4, false},
		{"ipv6", makeIPv6(), 6, false},
		{"unknown", []byte{0x11, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := IPVersion(c.pkt)
			if c.err && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !c.err && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
}

// makeIPv4 returns a minimal valid-looking IPv4 header (20 bytes, v4).
func makeIPv4() []byte {
	b := make([]byte, 20)
	b[0] = 0x45 // version 4, IHL 5
	b[2], b[3] = 0x00, 0x14
	return b
}

// makeIPv6 returns a minimal valid-looking IPv6 header (40 bytes, v6).
func makeIPv6() []byte {
	b := make([]byte, 40)
	b[0] = 0x60 // version 6
	return b
}
