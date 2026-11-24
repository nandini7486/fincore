// Package users creates FinCore users. In V1 users are created by an
// operator via fincorectl; there is no public sign-up endpoint.
package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nandini7486/fincore/backend/internal/audit"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
)

var (
	ErrInvalidInput = errors.New("invalid input")
	ErrEmailTaken   = errors.New("email already registered")
)

type NewUser struct {
	Name  string
	Email string
	Role  auth.Role
}

// Create inserts a user with a fresh API token and writes a user.create
// audit record. Call it inside a transaction so both commit together.
// The returned token is the only time the raw token is available.
func Create(ctx context.Context, tx db.DBTX, in NewUser) (auth.Identity, string, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.TrimSpace(in.Email)
	if in.Role == "" {
		in.Role = auth.RoleUser
	}
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 200 {
		return auth.Identity{}, "", fmt.Errorf("%w: name must be 1-200 characters", ErrInvalidInput)
	}
	if len(in.Email) < 3 || len(in.Email) > 320 || !strings.Contains(in.Email, "@") {
		return auth.Identity{}, "", fmt.Errorf("%w: email is not valid", ErrInvalidInput)
	}
	if !in.Role.Valid() {
		return auth.Identity{}, "", fmt.Errorf("%w: role must be USER or ADMIN", ErrInvalidInput)
	}

	token, err := auth.GenerateToken()
	if err != nil {
		return auth.Identity{}, "", err
	}

	id := auth.Identity{Name: in.Name, Email: in.Email, Role: in.Role}
	err = tx.QueryRow(ctx, `
		INSERT INTO users (name, email, role, api_token_hash)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		in.Name, in.Email, string(in.Role), auth.HashToken(token),
	).Scan(&id.UserID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_lower_key" {
			return auth.Identity{}, "", ErrEmailTaken
		}
		return auth.Identity{}, "", fmt.Errorf("insert user: %w", err)
	}

	err = audit.Write(ctx, tx, audit.Entry{
		Action:       "user.create",
		ResourceType: "user",
		ResourceID:   id.UserID,
		Outcome:      audit.Success,
		Details:      map[string]any{"role": string(in.Role)},
	})
	if err != nil {
		return auth.Identity{}, "", err
	}
	return id, token, nil
}
