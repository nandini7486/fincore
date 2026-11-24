package idempotency_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/idempotency"
)

func TestValidKey(t *testing.T) {
	for k, want := range map[string]bool{
		"6a3b6d9c-2f1e-4c8a-9b7d-1e2f3a4b5c6d": true,
		"a":                                    true,
		strings.Repeat("k", 255):               true,
		"":                                     false,
		strings.Repeat("k", 256):               false,
		"has space":                            false,
		"tab\there":                            false,
		"new\nline":                            false,
		"ünïcode":                              false,
	} {
		if got := idempotency.ValidKey(k); got != want {
			t.Errorf("ValidKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestHash(t *testing.T) {
	base := idempotency.Hash("payment.create", "a", "b", "100", "INR")
	if len(base) != 32 {
		t.Fatalf("hash length = %d, want 32 (SHA-256)", len(base))
	}
	if !bytes.Equal(base, idempotency.Hash("payment.create", "a", "b", "100", "INR")) {
		t.Fatal("hash is not deterministic")
	}
	for name, other := range map[string][]byte{
		"different operation": idempotency.Hash("topup.create", "a", "b", "100", "INR"),
		"different amount":    idempotency.Hash("payment.create", "a", "b", "101", "INR"),
		"swapped accounts":    idempotency.Hash("payment.create", "b", "a", "100", "INR"),
		// Without length prefixes these two would collide.
		"shifted boundary": idempotency.Hash("payment.create", "ab", "", "100", "INR"),
	} {
		if bytes.Equal(base, other) {
			t.Errorf("%s: hash collides with base", name)
		}
	}
	if !bytes.Equal(idempotency.Hash("x", "ab", "c"), idempotency.Hash("x", "ab", "c")) ||
		bytes.Equal(idempotency.Hash("x", "ab", "c"), idempotency.Hash("x", "a", "bc")) {
		t.Fatal("field boundaries must be part of the hash")
	}
	if idempotency.CanonicalID("0B8F6A52-1C3E-4D7A-9F2B-5E6C7D8A9B01") != "0b8f6a52-1c3e-4d7a-9f2b-5e6c7d8a9b01" {
		t.Fatal("CanonicalID must lowercase")
	}
}
