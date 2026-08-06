// Package config resolves RIPWS authentication credentials from a JSON file
// and/or command-line flags.
//
// The server maps a client's UUIDv4 path to the pre-shared key expected in
// its Authorization: Bearer header. Multiple clients are supported via a JSON
// file of the form:
//
//	{
//	  "f47ac10b-58cc-4372-a567-0e02b2c3d479": "8x/P9...44-char-base64...=",
//	  "..."
//	}
//
// For single-client deployments or tests, --uuid / --psk flags may be used
// instead (or in addition; flags take precedence).
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
)

// ErrInvalidUUID is returned when a configured key is not a valid UUIDv4.
var ErrInvalidUUID = errors.New("ripws: invalid UUIDv4")

// ErrInvalidPSK is returned when a PSK is not a 32-byte value base64-encoded
// to 44 characters, as mandated by SPEC 2.2.
var ErrInvalidPSK = errors.New("ripws: PSK must be 32 bytes base64-encoded (44 chars)")

// PSKLen is the required PSK entropy in bytes before base64 encoding.
const PSKLen = 32

// Credentials is the resolved authentication table keyed by canonical UUID.
type Credentials struct {
	// Keys maps lowercase UUID string -> raw PSK string (base64, 44 chars).
	Keys map[string]string `json:"keys"`
}

// New returns an empty Credentials.
func New() *Credentials { return &Credentials{Keys: make(map[string]string)} }

// Add validates and inserts a (uuid, psk) pair. The UUID is canonicalized to
// lower case. It returns ErrInvalidUUID / ErrInvalidPSK on bad input.
func (c *Credentials) Add(id, psk string) error {
	u, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidUUID, err)
	}
	if err := ValidatePSK(psk); err != nil {
		return err
	}
	c.Keys[u.String()] = psk
	return nil
}

// Lookup returns the PSK for the given UUID path element and a bool indicating
// presence. The input is trimmed of any leading '/'.
func (c *Credentials) Lookup(path string) (string, bool) {
	path = strings.Trim(path, "/")
	psk, ok := c.Keys[path]
	return psk, ok
}

// ValidatePSK enforces SPEC 2.2: 32 random bytes, base64-encoded to 44 chars.
func ValidatePSK(psk string) error {
	if len(psk) != 44 {
		return ErrInvalidPSK
	}
	b, err := base64.StdEncoding.DecodeString(psk)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPSK, err)
	}
	if len(b) != PSKLen {
		return ErrInvalidPSK
	}
	return nil
}

// LoadFile reads a JSON credentials file. The file may be either:
//   - an object mapping UUID -> PSK directly, or
//   - an object with a "keys" field of the same shape.
//
// Entries that fail validation are skipped silently? No: SPEC compliance
// demands we fail closed, so any invalid entry returns an error.
func LoadFile(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ripws: read config: %w", err)
	}

	// Try the wrapper shape first, fall back to a flat map.
	var wrapper struct {
		Keys map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Keys != nil {
		c := New()
		for id, psk := range wrapper.Keys {
			if err := c.Add(id, psk); err != nil {
				return nil, fmt.Errorf("ripws: config entry %q: %w", id, err)
			}
		}
		return c, nil
	}

	var flat map[string]string
	if err := json.Unmarshal(data, &flat); err != nil {
		return nil, fmt.Errorf("ripws: parse config: %w", err)
	}
	c := New()
	for id, psk := range flat {
		if err := c.Add(id, psk); err != nil {
			return nil, fmt.Errorf("ripws: config entry %q: %w", id, err)
		}
	}
	return c, nil
}

// MergeFlags injects a single --uuid/--psk pair into c, taking precedence over
// any same-UUID entry from a loaded file. Empty id or psk is a no-op.
func MergeFlags(c *Credentials, id, psk string) error {
	if id == "" || psk == "" {
		return nil
	}
	return c.Add(id, psk)
}

// GeneratePSK produces a fresh 32-byte PSK encoded as standard base64 (44
// chars). It is used by the test tooling to mint credentials.
func GeneratePSK() (string, error) {
	buf := make([]byte, PSKLen)
	if _, err := randRead(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}
