// Package accounts creates and reads FinCore accounts (Blueprint FR-09).
//
// An account's balance is a projection of the ledger. This package never
// changes it: only the payment and top-up paths do, in the same transaction
// as the matching ledger entries (Phase 2c).
package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/audit"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
)

// SupportedCurrency is the only currency in V1 (Blueprint §1.3).
const SupportedCurrency = "INR"

var (
	ErrInvalidInput = errors.New("invalid input")
	ErrNotFound     = errors.New("account not found")
	// ErrNotOwner means the account exists but belongs to someone else.
	// Callers must not reveal this to the client: respond as for ErrNotFound.
	ErrNotOwner = errors.New("account belongs to another user")
)

type Account struct {
	ID        string
	UserID    *string // nil only for the treasury account
	Type      string  // USER or TREASURY
	Currency  string
	Balance   int64 // minor units (paise)
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const columns = `id, user_id, type, currency, balance, status, created_at, updated_at`

func scan(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.UserID, &a.Type, &a.Currency, &a.Balance, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// Create opens a USER account for ownerID with a zero balance and writes an
// account.create audit record. Call it inside a transaction so the account
// and its audit record commit together (Blueprint §8.3).
func Create(ctx context.Context, tx db.DBTX, ownerID, currency string) (Account, error) {
	if currency == "" {
		currency = SupportedCurrency
	}
	if currency != SupportedCurrency {
		return Account{}, fmt.Errorf("%w: currency must be %s", ErrInvalidInput, SupportedCurrency)
	}

	// Balance and status come from column defaults (0, ACTIVE): the API
	// never lets a client choose a starting balance.
	a, err := scan(tx.QueryRow(ctx, `
		INSERT INTO accounts (user_id, type, currency)
		VALUES ($1, 'USER', $2)
		RETURNING `+columns,
		ownerID, currency))
	if err != nil {
		return Account{}, fmt.Errorf("insert account: %w", err)
	}

	err = audit.Write(ctx, tx, audit.Entry{
		ActorUserID:  &ownerID,
		Action:       "account.create",
		ResourceType: "account",
		ResourceID:   a.ID,
		Outcome:      audit.Success,
		Details:      map[string]any{"currency": a.Currency},
	})
	if err != nil {
		return Account{}, err
	}
	return a, nil
}

// GetForOwner returns the account if ownerID owns it. It returns ErrNotFound
// if no such account exists, and ErrNotOwner if it belongs to someone else
// (including the treasury, which has no owner).
func GetForOwner(ctx context.Context, q db.DBTX, accountID, ownerID string) (Account, error) {
	a, err := scan(q.QueryRow(ctx, `SELECT `+columns+` FROM accounts WHERE id = $1`, accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	if a.UserID == nil || *a.UserID != ownerID {
		return Account{}, ErrNotOwner
	}
	return a, nil
}
