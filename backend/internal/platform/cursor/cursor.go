// Package cursor implements the opaque keyset-pagination cursor shared by
// list endpoints (payment history, notifications). See ADR 0002.
package cursor

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/nandini7486/fincore/backend/internal/platform/ids"
)

// ErrInvalid is returned for any cursor that Encode did not produce.
var ErrInvalid = errors.New("invalid cursor")

// Cursor marks the last item of a page. The next page continues with items
// strictly older in (created_at, id) order; id breaks ties between items
// created in the same microsecond, so nothing is skipped or repeated.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Encode returns the opaque string handed to clients. Its format is an
// implementation detail clients must not rely on.
func (c Cursor) Encode() string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// Decode parses a cursor from Encode. Any tampered or malformed value
// returns ErrInvalid.
func Decode(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrInvalid
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || !ids.IsUUID(id) {
		return Cursor{}, ErrInvalid
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Cursor{}, ErrInvalid
	}
	return Cursor{CreatedAt: t, ID: id}, nil
}
