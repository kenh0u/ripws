package config

import "crypto/rand"

// randRead is a small indirection so tests can stub the entropy source.
func randRead(b []byte) (int, error) { return rand.Read(b) }
