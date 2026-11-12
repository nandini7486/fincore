// Package auth implements FinCore's minimal V1 bearer-token authentication
// (Blueprint §16 #9). Tokens are random secrets shown once at creation;
// only their SHA-256 hash is stored.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/platform/db"
)

type Role string

const (
	RoleUser  Role = "USER"
	RoleAdmin Role = "ADMIN"
)

func (r Role) Valid() bool { return r == RoleUser || r == RoleAdmin }

// Identity is the authenticated caller. It is always derived from the
// token, never from request fields (NFR-05).
type Identity struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Role   Role   `json:"role"`
}

const tokenPrefix = "fct_"

// GenerateToken returns a new random bearer token (256 bits of entropy).
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the SHA-256 hash stored in users.api_token_hash.
// A fast hash is appropriate here because tokens are high-entropy random
// values, unlike passwords.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

var ErrUnknownToken = errors.New("unknown token")

// LookupByToken returns the identity owning token.
func LookupByToken(ctx context.Context, q db.DBTX, token string) (Identity, error) {
	var id Identity
	err := q.QueryRow(ctx,
		`SELECT id, name, email, role FROM users WHERE api_token_hash = $1`,
		HashToken(token),
	).Scan(&id.UserID, &id.Name, &id.Email, &id.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrUnknownToken
	}
	if err != nil {
		return Identity{}, fmt.Errorf("lookup token: %w", err)
	}
	return id, nil
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

type identityKey struct{}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the authenticated identity set by Middleware.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}
