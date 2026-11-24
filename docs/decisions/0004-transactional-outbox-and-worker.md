# 0004: Transactional outbox with an at-least-once worker

- **Status:** Accepted
- **Date:** 2026-11-24
- **Phase:** 5 (outbox, worker, notifications)

## Context

When money moves, other things must follow: today an in-app notification,
later webhooks, emails and the AI service. Doing them inside the request is a
**dual write** across two systems:

- the payment commits, then the process dies before notifying → the
  notification is **lost**;
- the notification goes out, then the payment rolls back → it announced money
  that **never moved**.

## Decision

1. **Write the event in the payment's transaction.** A successful transfer or
   top-up inserts a `payment.succeeded` / `topup.succeeded` row into
   `outbox_events` in the same transaction as the balances and ledger. The
   event exists if and only if the money moved. Failed payments write none.
2. **A separate worker delivers events** (`cmd/worker`). It claims one due
   event at a time with `SELECT … FOR UPDATE SKIP LOCKED`, so any number of
   workers can run: each takes a different event, and an event being handled
   is invisible to the others.
3. **Handler and "processed" mark in one transaction.** The handler's
   database writes and `processed_at = now()` commit together. If the worker
   dies mid-way, both roll back and the event is picked up again.
4. **Failures are isolated and retried.** The handler runs inside a
   savepoint. On error (or panic, or an unknown event type) its writes are
   undone, and `attempts`, `last_error` and `next_attempt_at` are recorded.
   Backoff doubles from 1s, capped at 5 minutes. A bad event never blocks the
   ones behind it.
5. **Handlers must be idempotent**, because delivery is at-least-once (a
   future handler with an external side effect, such as a webhook, can repeat
   its effect if the worker dies after the call but before commit). The
   notifications handler relies on `UNIQUE (event_id)` with
   `ON CONFLICT DO NOTHING`.
6. The same worker deletes expired idempotency keys in batches of 1,000
   (`SKIP LOCKED`, so it never waits on a key being claimed).

## Consequences

- Notifications are eventually consistent: they appear within about one poll
  interval (default 1s) after the payment, not inside the request.
- The payment path never depends on the notification path: a broken consumer
  only delays notifications, it cannot fail a payment.
- There is no dead-letter state yet: a permanently failing event keeps
  retrying every 5 minutes, visible through `attempts` and `last_error`.
  Alerting on it belongs with metrics (Phase 6/8).
- Processed events are kept as a history; pruning them is future work.

## Evidence

- Payment tests: exactly one event per successful payment, none for failed
  ones, none after any injected failure or a `kill -9` of the API.
- Worker tests: delivery and marking; retry with backoff after a failure,
  with the failed handler's writes rolled back; panicking and unknown events
  do not block others; 6 competing workers each hold a different event at
  the same moment and deliver 300 events exactly once;
  a connection killed mid-handler leaves no trace and the event is
  redelivered once; `Run` keeps polling until cancelled.
- Notifications: correct messages for the receiver only (Indian digit
  grouping), redelivery never duplicates, bad events are recorded with a clear
  reason.
- Process test of the real worker binary: delivers notifications, cleans up
  expired keys, exits 0 on `SIGTERM`.
- Mutations caught: non-idempotent consumer, failed handler's writes kept,
  events written for failed payments, and `SKIP LOCKED` removed (the
  competing-workers test holds all 6 workers at a barrier until each has a
  different event at the same moment, which is impossible without it).
