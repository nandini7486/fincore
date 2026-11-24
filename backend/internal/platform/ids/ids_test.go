package ids_test

import (
	"testing"

	"github.com/nandini7486/fincore/backend/internal/platform/ids"
)

func TestIsUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"0b8f6a52-1c3e-4d7a-9f2b-5e6c7d8a9b01":  true,
		"0B8F6A52-1C3E-4D7A-9F2B-5E6C7D8A9B01":  true,
		"":                                      false,
		"not-a-uuid":                            false,
		"0b8f6a521c3e4d7a9f2b5e6c7d8a9b01":      false, // no hyphens
		"0b8f6a52-1c3e-4d7a-9f2b-5e6c7d8a9b0":   false, // too short
		"0b8f6a52-1c3e-4d7a-9f2b-5e6c7d8a9b011": false, // too long
		"0b8f6a52-1c3e-4d7a-9f2b_5e6c7d8a9b01":  false, // wrong separator
		"zb8f6a52-1c3e-4d7a-9f2b-5e6c7d8a9b01":  false, // non-hex
		"1' OR '1'='1-xxxx-xxxx-xxxx-xxxxxxxxx": false,
	} {
		if got := ids.IsUUID(s); got != want {
			t.Errorf("IsUUID(%q) = %v, want %v", s, got, want)
		}
	}
}
