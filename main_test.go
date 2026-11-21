package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPIDFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ripws0.pid")

	if err := writePIDFile(path, 12345, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, ifc := parsePIDFile(data)
	if pid != 12345 || ifc != "" {
		t.Fatalf("got (%d, %q), want (12345, \"\")", pid, ifc)
	}

	if err := writePIDFile(path, 67890, "tun0"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, ifc = parsePIDFile(data)
	if pid != 67890 || ifc != "tun0" {
		t.Fatalf("got (%d, %q), want (67890, \"tun0\")", pid, ifc)
	}

	for _, bad := range []string{"", "abc\n", "-1\n", "0\ntun0\n"} {
		if p, _ := parsePIDFile([]byte(bad)); p != 0 {
			t.Errorf("parsePIDFile(%q) pid = %d, want 0", bad, p)
		}
	}
}
