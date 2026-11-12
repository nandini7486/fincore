// Package migrations holds FinCore's versioned SQL schema migrations and
// a small helper to apply them with goose.
package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var FS embed.FS

// NewProvider returns a goose provider for the embedded migrations.
func NewProvider(db *sql.DB) (*goose.Provider, error) {
	p, err := goose.NewProvider(goose.DialectPostgres, db, FS)
	if err != nil {
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	return p, nil
}
