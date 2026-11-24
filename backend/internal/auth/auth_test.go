package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer fct_abc", "fct_abc", true},
		{"bearer fct_abc", "fct_abc", true},
		{"Bearer   fct_abc  ", "fct_abc", true},
		{"", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Basic dXNlcjpwYXNz", "", false},
		{"fct_abc", "", false},
	}
	for _, tt := range tests {
		got, ok := bearerToken(tt.header)
		if got != tt.want || ok != tt.ok {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tt.header, got, ok, tt.want, tt.ok)
		}
	}
}

func TestGenerateToken(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two generated tokens are equal")
	}
	if !strings.HasPrefix(a, tokenPrefix) {
		t.Fatalf("token %q missing prefix %q", a, tokenPrefix)
	}
	if len(a) != len(tokenPrefix)+43 { // 32 bytes, base64url without padding
		t.Fatalf("token length = %d", len(a))
	}
}

func TestHashToken(t *testing.T) {
	h1 := HashToken("fct_x")
	if len(h1) != 32 {
		t.Fatalf("hash length = %d, want 32", len(h1))
	}
	if !bytes.Equal(h1, HashToken("fct_x")) {
		t.Fatal("hash is not deterministic")
	}
	if bytes.Equal(h1, HashToken("fct_y")) {
		t.Fatal("different tokens produced the same hash")
	}
}

func TestRoleValid(t *testing.T) {
	if !RoleUser.Valid() || !RoleAdmin.Valid() || Role("ROOT").Valid() {
		t.Fatal("Role.Valid incorrect")
	}
}
