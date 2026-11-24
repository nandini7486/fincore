# 0003: Claim the Idempotency-Key inside the payment transaction

- **Status:** Accepted
- **Date:** 2026-11-24
- **Phase:** 3a (idempotency)

## Context

Networks fail after the server has done the work: the client times out, does
not know whether the payment happened, and retries. Users also double-tap
buttons. Without protection, each copy moves money (NFR-01).

The client therefore sends an `Idempotency-Key` (a fresh UUID per logical
payment, reused on every retry). The question is how to make sure only one
copy does the work.

### The tempting approach, and why it fails

```
if key exists: return stored response
else: insert key; process payment; save response
```

Two copies arriving together both see "key does not exist" before either has
committed, and both charge the customer. This is a check-then-act race.

FinCore has a test that demonstrates it: with this naive version swapped in,
`TestDuplicatesWhileFirstRequestIsInFlight` double-charged in **10 of 10**
runs.

## Decision

The claim is the first write in the payment transaction (after setting the
lock and statement timeouts):

```sql
INSERT INTO idempotency_records (user_id, key, request_hash) VALUES (...)
ON CONFLICT (user_id, key) DO UPDATE ... WHERE idempotency_records.expires_at <= now()
```

- The primary key `(user_id, key)` forces a second copy's `INSERT` to **wait**
  until the first transaction ends.
  - First commits: the second sees the stored record and **replays** the
    stored status and body, with `Idempotent-Replayed: true`.
  - First rolls back (timeout, crash, error): the second's insert succeeds
    and it processes the payment normally.
- The response is saved in the same transaction as the money movement, so a
  committed key always has its response. There is never a stale
  "in progress" record to clean up after a crash.
- The fingerprint is SHA-256 of the **operation** plus **parsed** fields
  (amount in paise, lowercase IDs, currency), length-prefixed. So `"5.0"` and
  `"5.00"` are the same request, and a key reused across endpoints or with a
  different amount is a `409 IDEMPOTENCY_KEY_REUSED` (audited as
  `idempotency.conflict`).
- Business-rule failures (`422`) are stored and replayed: a retry of a payment
  that failed for insufficient funds returns the same `422` and the same
  `FAILED` payment, even if money has arrived since. A new attempt needs a new
  key.
- Validation errors (`400`) happen before the transaction and are never
  stored.
- Keys are scoped per user and expire after 24 hours; an expired key is taken
  over atomically by the same `INSERT`.

## Consequences

- Clients must send a new key per payment and the same key on retries. The
  API requires the header and explains this in the `400` message.
- A duplicate waits for the first request instead of failing fast. The wait is
  bounded by `lock_timeout` (2s), after which it gets a retryable `503`.
- `response_body` is `jsonb`, so a replayed body is the same JSON but may
  differ in key order or whitespace. Clients parse JSON, so this is
  acceptable.
- Expired records still need deleting; the cleanup job arrives with the
  background worker in Phase 5.

## Evidence

- 12 HTTP tests and 2 unit tests: replay, equivalent body, conflict, cross-endpoint conflict,
  replayed `422`, per-user scope, header validation, expiry, a `503` not
  burning the key, and three concurrency tests.
- The concurrency tests passed 100+ repeated runs with the race detector.
- The original random-timing concurrency test caught the naive bug only
  4 times in 30. That is why the deterministic in-flight test exists:
  concurrency tests must force the interleaving, not hope for it.
