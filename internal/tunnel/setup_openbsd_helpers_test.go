package tunnel

import (
	"net/netip"
	"testing"
)

func TestParseOpenBSDAddress(t *testing.T) {
	cases := []struct {
		in     string
		cidr   string
		ip     string
		isV6   bool
		wantOk bool
	}{
		{"172.29.207.2/24", "172.29.207.2/24", "172.29.207.2", false, true},
		{"10.7.0.1", "10.7.0.1/32", "10.7.0.1", false, true},
		{"fd00::2/64", "fd00::2/64", "fd00::2", true, true},
		{"2001:db8::1", "2001:db8::1/128", "2001:db8::1", true, true},
		{"not-an-ip", "", "", false, false},
	}
	for _, tc := range cases {
		cidr, ip, isV6, err := parseOpenBSDAddress(tc.in)
		if (err == nil) != tc.wantOk {
			t.Errorf("parseOpenBSDAddress(%q) err=%v, wantOk=%v", tc.in, err, tc.wantOk)
			continue
		}
		if err != nil {
			continue
		}
		if cidr != tc.cidr || ip != tc.ip || isV6 != tc.isV6 {
			t.Errorf("parseOpenBSDAddress(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, cidr, ip, isV6, tc.cidr, tc.ip, tc.isV6)
		}
	}
}

func TestExpandOpenBSDRoutes(t *testing.T) {
	m4, targets4, err := expandOpenBSDRoutes("0.0.0.0/0")
	if err != nil || m4.Bits() != 0 || len(targets4) != 2 ||
		targets4[0].String() != "0.0.0.0/1" || targets4[1].String() != "128.0.0.0/1" {
		t.Fatalf("v4 default expand = (%v, %v, %v)", m4, targets4, err)
	}

	m6, targets6, err := expandOpenBSDRoutes("::/0")
	if err != nil || m6.Bits() != 0 || len(targets6) != 2 ||
		targets6[0].String() != "::/1" || targets6[1].String() != "8000::/1" {
		t.Fatalf("v6 default expand = (%v, %v, %v)", m6, targets6, err)
	}

	mSub, targetsSub, err := expandOpenBSDRoutes("10.205.115.123/24")
	if err != nil || mSub.String() != "10.205.115.0/24" || len(targetsSub) != 1 || targetsSub[0] != mSub {
		t.Fatalf("masked subnet = (%v, %v, %v)", mSub, targetsSub, err)
	}

	if _, _, err := expandOpenBSDRoutes("bad"); err == nil {
		t.Error("expected error on bad CIDR")
	}
}

func TestParseOpenBSDRouteGet(t *testing.T) {
	sample := []byte(`   route to: 203.0.113.10
destination: 0.0.0.0
       mask: 0.0.0.0
    gateway: 10.205.115.1
  interface: vmx0
 if address: 10.205.115.123
   priority: 8 (static)
      flags: <UP,GATEWAY,DONE,STATIC>
`)
	gw, ifc := parseOpenBSDRouteGet(sample)
	if gw != "10.205.115.1" || ifc != "vmx0" {
		t.Fatalf("got gw=%q iface=%q, want 10.205.115.1 / vmx0", gw, ifc)
	}

	onLink := []byte(`   route to: 10.205.115.5
destination: 10.205.115.0
       mask: 255.255.255.0
    gateway: link#1
  interface: vio0
`)
	gw2, ifc2 := parseOpenBSDRouteGet(onLink)
	if gw2 != "" || ifc2 != "vio0" {
		t.Fatalf("on-link got gw=%q iface=%q, want empty / vio0", gw2, ifc2)
	}

	if got := openBSDHostCIDR(netip.MustParseAddr("::ffff:203.0.113.10")); got != "203.0.113.10/32" {
		t.Fatalf("hostCIDR = %q, want 203.0.113.10/32", got)
	}
}
