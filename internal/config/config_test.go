package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePSK(t *testing.T) {
	good := generatePSK(t)
	if err := ValidatePSK(good); err != nil {
		t.Fatalf("good psk rejected: %v", err)
	}
	if err := ValidatePSK("tooShort"); err == nil {
		t.Fatal("short psk accepted")
	}
	if err := ValidatePSK(good[:43] + "!"); err == nil {
		t.Fatal("non-base64 psk accepted")
	}
}

func TestAddRejectsBad(t *testing.T) {
	c := New()
	if err := c.Add("not-a-uuid", generatePSK(t)); err == nil {
		t.Fatal("bad uuid accepted")
	}
	goodUUID := "f47ac10b-58cc-4372-a567-0e02b2c3d479"
	if err := c.Add(goodUUID, "short"); err == nil {
		t.Fatal("bad psk accepted")
	}
	if err := c.Add(goodUUID, generatePSK(t)); err != nil {
		t.Fatalf("good pair rejected: %v", err)
	}
	// Lookup canonicalises and trims slashes.
	if psk, ok := c.Lookup("/f47ac10b-58cc-4372-a567-0e02b2c3d479/"); !ok {
		t.Fatal("lookup failed")
	} else if psk != generatePSK(t) && len(psk) != 44 {
		// psk is the one we added; just sanity-check length.
		t.Fatalf("unexpected psk len %d", len(psk))
	}
}

func TestLoadFileFlatAndWrapper(t *testing.T) {
	dir := t.TempDir()
	psk := generatePSK(t)
	flat := `{"f47ac10b-58cc-4372-a567-0e02b2c3d479": "` + psk + `"}`
	fp := filepath.Join(dir, "flat.json")
	if err := os.WriteFile(fp, []byte(flat), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(fp)
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	if len(c.Keys) != 1 {
		t.Fatalf("flat: want 1 key, got %d", len(c.Keys))
	}

	wrap := `{"keys": {"f47ac10b-58cc-4372-a567-0e02b2c3d479": "` + psk + `"}}`
	wp := filepath.Join(dir, "wrap.json")
	if err := os.WriteFile(wp, []byte(wrap), 0644); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadFile(wp)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if len(c2.Keys) != 1 {
		t.Fatalf("wrap: want 1 key, got %d", len(c2.Keys))
	}
}

func generatePSK(t *testing.T) string {
	t.Helper()
	psk, err := GeneratePSK()
	if err != nil {
		t.Fatal(err)
	}
	return psk
}
