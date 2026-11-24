package idempotency_test

import (
	"context"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/idempotency"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
)

// The worker's cleanup deletes only records whose 24-hour window has passed,
// in batches, running as the API's restricted role.
func TestDeleteExpired(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tdb.Owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, name, email, role, api_token_hash)
	      VALUES ('00000000-0000-4000-8000-0000000000aa', 'A', 'a@example.com', 'USER', decode(repeat('ab', 32), 'hex'))`)

	// 2,500 expired (more than two batches) and 7 still live, one of which
	// expires a second from now.
	exec(`INSERT INTO idempotency_records (user_id, key, request_hash, created_at, expires_at)
	      SELECT '00000000-0000-4000-8000-0000000000aa', 'old-' || g, decode(repeat('00', 32), 'hex'),
	             now() - interval '2 days', now() - interval '1 day'
	      FROM generate_series(1, 2500) g`)
	exec(`INSERT INTO idempotency_records (user_id, key, request_hash, expires_at)
	      SELECT '00000000-0000-4000-8000-0000000000aa', 'live-' || g, decode(repeat('00', 32), 'hex'),
	             now() + interval '1 second' * g
	      FROM generate_series(1, 7) g`)

	deleted, err := idempotency.DeleteExpired(ctx, tdb.App)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2500 {
		t.Fatalf("deleted = %d, want 2500", deleted)
	}
	var expired, live int
	if err := tdb.Owner.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE key LIKE 'old-%'), count(*) FILTER (WHERE key LIKE 'live-%')
		FROM idempotency_records`).Scan(&expired, &live); err != nil {
		t.Fatal(err)
	}
	if expired != 0 || live != 7 {
		t.Fatalf("remaining expired=%d live=%d, want 0 and 7", expired, live)
	}

	// Nothing more to delete.
	if deleted, err := idempotency.DeleteExpired(ctx, tdb.App); err != nil || deleted != 0 {
		t.Fatalf("second run deleted %d, %v; want 0", deleted, err)
	}
}
