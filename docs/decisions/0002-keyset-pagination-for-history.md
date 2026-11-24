# 0002: Keyset (cursor) pagination for payment history

- **Status:** Accepted
- **Date:** 2026-11-24
- **Phase:** 2d (payment history)

## Context

`GET /api/v1/accounts/{account_id}/payments` returns an account's payments,
newest first, a page at a time (FR-07). The obvious approach,
`ORDER BY created_at DESC LIMIT 20 OFFSET 40`, has two problems:

1. **Wrong results while data changes.** If new payments arrive between page
   requests, every row shifts down: the client sees some payments twice.
   (Payments are never deleted, so the opposite problem, skipped rows, does
   not arise here, but duplicates on a statement are bad enough.)
2. **Slow deep pages.** `OFFSET n` makes PostgreSQL read and throw away `n`
   rows, so page 500 is about 500 times more work than page 1.

## Decision

- Paginate on the key `(created_at, id)`. The next page continues with
  `WHERE (created_at, id) < (last_created_at, last_id)`. `id` breaks ties
  between payments created in the same microsecond.
- The cursor sent to clients is opaque: base64url of
  `<RFC3339Nano timestamp>|<uuid>`. It is strictly validated; anything else
  returns `400 INVALID_CURSOR`.
- `limit` defaults to 20, accepts 1-100, and returns `400 INVALID_LIMIT`
  otherwise. One extra row is fetched to decide whether `next_cursor` is set.
- History includes payments the account sent or received, so the query is a
  `UNION ALL` of two branches, each served by its own index (schema design
  §2.3): `(sender_account_id, created_at DESC, id DESC)` and the receiver
  equivalent. Failed payments are included; they have no ledger entries, which
  is why history is read from `payments`, not `ledger_entries`.

## Evidence

Measured locally on PostgreSQL 16 with 200,000 payments across 1,000
accounts:

```
Limit
  ->  Merge Append   (Sort Key: created_at DESC, id DESC)
        ->  Limit -> Index Only Scan using payments_sender_history_idx    (rows=11)
        ->  Limit -> Index Only Scan using payments_receiver_history_idx  (rows=11)
Execution Time: 0.28 ms   (a deep page with a cursor: 0.24 ms)
```

Each branch reads only the rows it returns; no sort, no table scan.

Tests prove the behaviour: paging at limits 1-100 matches a reference query
exactly, five payments with the same timestamp page correctly, and new
payments arriving mid-scroll do not shift later pages. Removing `id` from the
cursor condition makes the tie test fail (it returned 1 of 5 payments).

## Also used by

`GET /api/v1/notifications` uses the same cursor (`internal/platform/cursor`)
and pagination rules, served by `notifications (user_id, created_at DESC, id DESC)`
(migration 10): 0.25 ms per page on 200,000 notifications, index scan, no sort.

## Consequences

- Clients can only move forward page by page; there is no "jump to page 37".
  That fits statements and feeds.
- The cursor format can change later without breaking clients, because it is
  opaque.
