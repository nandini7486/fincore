# FinCore --- Project Blueprint

> **Document type:** Product and engineering specification\
> **Project:** FinCore\
> **Version:** 0.2 --- Reviewed design\
> **Status:** Foundational decisions confirmed (see §16); ready for Phase 0\
> **Primary goal:** Learn and demonstrate end-to-end backend engineering
> by building a reliable, free-first payment-processing platform with an
> applied AI intelligence service.

### Changes from v0.1

-   Idempotency claim moved **inside** the payment transaction (fixes a
    race in the v0.1 flow).
-   Business-rule failures are now persisted as `FAILED` payments so
    retries replay the same outcome.
-   Authorization corrected: caller must own the **sender** account only.
-   Added a system treasury account and a top-up flow so money has a
    double-entry source; global ledger sum must be zero.
-   Added a mandatory, append-only **audit log** (§8.3).
-   Added account, balance, top-up, and health endpoints.
-   Money stored as `BIGINT` minor units (paise).
-   §16 decisions finalized; concrete tool choices made.
-   Roadmap reordered: Docker Compose, CI, tests, minimal auth, and audit
    logging start early. V1 scope cut; NATS, MinIO, React, AI moved to V2.

------------------------------------------------------------------------

## 1. Project overview

FinCore is a learning-focused payment infrastructure platform. Its first
version will simulate account-to-account transfers between accounts held
inside FinCore. It will provide APIs to create accounts, fund them via a
controlled top-up, create payments, and retrieve payment details and
account payment history.

The project is intended to be built from fundamentals through
implementation, testing, deployment, monitoring, and operational
documentation. It should resemble the engineering practices used in
production systems without requiring paid infrastructure.

FinCore is a **simulation and portfolio project**. It will not move real
money or connect to real banking rails in its initial version.
Payment-provider integrations may be explored later using sandbox
environments.

### 1.1 Core goals

-   Build a correct payment-processing workflow.
-   Debit the sender and credit the receiver atomically.
-   Maintain an auditable, double-entry ledger.
-   Maintain an append-only audit log of every security- and
    money-relevant action.
-   Prevent duplicate processing when clients retry requests.
-   Handle concurrent payment requests safely.
-   Provide efficient payment retrieval APIs.
-   Learn Go backend engineering, PostgreSQL, Python, applied ML/AI,
    testing, deployment, and observability.
-   Use free and open-source tools wherever practical.
-   Understand every major design and implementation decision rather
    than blindly accepting generated code.

### 1.2 Release scope

**V1 (Phases 0--6):** Go API, PostgreSQL schema, accounts, top-ups,
payments, ledger, idempotency, concurrency safety, audit log, minimal
auth, outbox + worker, Docker Compose, CI, structured logs, basic
metrics, automated tests, load test report.

**V2 (Phases 7+):** hardened auth, rate limiting, Prometheus/Grafana
dashboards, Python AI service, React dashboard, optional NATS, optional
hosted deployment.

### 1.3 Out of scope for V1

-   Real bank transfers, UPI rails, card processing, or custody of real
    funds.
-   Production-grade regulatory, compliance, KYC, AML, or PCI
    certification.
-   Multi-currency transfers or FX conversion (INR only).
-   Merchant/tenant accounts.
-   Multi-region deployment.
-   A large language model trained from scratch.
-   Microservices.
-   Paid managed infrastructure.
-   AI making autonomous financial decisions.

------------------------------------------------------------------------

## 2. Product scope and functional requirements

### FR-01: Initiate a payment

An authenticated user can request a transfer from an account they own to
any other active FinCore account in the same currency.

The request must include:

-   Sender account ID (UUID)
-   Receiver account ID (UUID)
-   Amount (decimal string)
-   Currency
-   Idempotency key supplied in the `Idempotency-Key` request header

The system validates the request before processing it.

### FR-02: Debit the sender

For a valid payment, the sender's account is debited by the requested
amount.

The system must verify, while holding the account lock, that the sender
account is active, owned by the caller, and has sufficient funds.
Balances may never become negative (except the system treasury account,
see FR-10).

### FR-03: Credit the receiver

The receiver's account is credited by exactly the amount debited from
the sender. The receiver must exist, be active, and have the same
currency. The caller does **not** need to own the receiver account.

The sender debit and receiver credit must be committed together. The
system must never commit only one side of a successful internal
transfer.

### FR-04: Record both sides in the ledger

Every successful transfer creates two linked ledger entries:

1.  A debit entry against the sender account.
2.  A credit entry against the receiver account.

Both entries reference the same payment ID and currency. The ledger is
the financial audit trail.

### FR-05: Create payment --- POST

`POST /api/v1/payments`

``` http
POST /api/v1/payments
Content-Type: application/json
Authorization: Bearer <token>
Idempotency-Key: 6a3b6d9c-2f1e-4c8a-9b7d-1e2f3a4b5c6d
```

``` json
{
  "sender_account_id": "0b8f6a52-1c3e-4d7a-9f2b-5e6c7d8a9b01",
  "receiver_account_id": "7c2d1e4f-8a9b-4c3d-b2e1-0f9e8d7c6b5a",
  "amount": "2000.00",
  "currency": "INR"
}
```

-   Amount is a decimal string at the API boundary, at most 2 decimal
    places for INR, strictly positive, and below a configured maximum.
-   Amount is stored as `BIGINT` minor units (paise): `"2000.00"` →
    `200000`. Binary floating-point is never used for money.
-   A new successful payment returns `201 Created`.
-   A business-rule failure returns `422` and is persisted as a `FAILED`
    payment (see §6.4).
-   A retry with the same key and identical request replays the original
    status code and body with header `Idempotent-Replayed: true`.

### FR-06: Retrieve a payment --- GET

`GET /api/v1/payments/{payment_id}`

Returns the payment's details and status. Visible to the owner of the
sender or receiver account. Otherwise returns `404`.

### FR-07: Retrieve account payment history --- GET

`GET /api/v1/accounts/{account_id}/payments?limit=20&cursor=...`

Returns payments involving the account, newest first, using keyset
(cursor) pagination on `(created_at, id)`. `limit` defaults to 20, max
100. The cursor is opaque (base64-encoded). Only the account owner may
read it; otherwise `404`.

### FR-08: Track payment status

Statuses:

-   `PROCESSING` --- reserved for future asynchronous workflows; not
    used by the V1 synchronous flow.
-   `SUCCESS` --- balances and ledger updated.
-   `FAILED` --- business rule rejected the payment; no balance or
    ledger change. A `failure_reason` code is stored (e.g.
    `INSUFFICIENT_FUNDS`, `ACCOUNT_INACTIVE`, `CURRENCY_MISMATCH`).

Status values are enforced with a database `CHECK` constraint. V1
payments are created directly in a final state; any future transition
(`PROCESSING` → `SUCCESS`/`FAILED`) must be explicit and validated.

### FR-09: Accounts

-   `POST /api/v1/accounts` --- create an account (currency `INR`) owned
    by the authenticated user. Starts with balance 0, status `ACTIVE`.
-   `GET /api/v1/accounts/{account_id}` --- return account details and
    current balance. Owner only; otherwise `404`.
-   Users are created by a seed script/admin command in V1 (no public
    sign-up endpoint).

### FR-10: Funding (top-up)

Money must come from somewhere in a double-entry system.

-   A single **system treasury account** exists per currency. It is the
    only account allowed to hold a negative balance. Its negative
    balance equals the total money issued into FinCore.
-   `POST /api/v1/accounts/{account_id}/topups` (admin role only,
    requires `Idempotency-Key`) moves money from treasury to a user
    account using the same transactional path as a payment: DEBIT
    treasury, CREDIT user account, payment type `TOPUP`.
-   Global invariant: **the sum of all ledger entries (credits positive,
    debits negative) is zero**.

### FR-11: Audit log

Every security- and money-relevant action writes an append-only audit
record (see §8.3). For money-moving actions, the audit record is written
in the **same database transaction** as the financial change.

### FR-12: Health

-   `GET /healthz` --- process is alive.
-   `GET /readyz` --- process can reach PostgreSQL.

------------------------------------------------------------------------

## 3. Non-functional requirements

### NFR-01: Duplicate transaction protection

-   Require an idempotency key for payment and top-up creation.
-   Uniqueness enforced in PostgreSQL on `(user_id, idempotency_key)`.
-   Store a SHA-256 hash of the canonical request (parsed fields in a
    fixed order, amount in minor units) with the record.
-   Same key + different request hash → `409 Conflict`.
-   Same key + same hash → do not debit or credit again; replay the
    stored status code and body.
-   The claim is made **inside** the payment transaction with
    `INSERT ... ON CONFLICT DO NOTHING`; see §6.3. An application-level
    "check then insert" is not sufficient under concurrency.
-   Retention: 24 hours, removed by a periodic cleanup job. After expiry
    a key may be reused.

### NFR-02: Concurrent transaction safety

-   Use PostgreSQL transactions (default `READ COMMITTED`) for all
    financial mutations.
-   Lock both account rows in one statement in deterministic order:
    `SELECT ... FROM accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`.
-   Enforce sufficient-funds validation while holding the lock.
-   Set a `lock_timeout` and `statement_timeout` on financial
    transactions; surface timeouts as retryable `503` errors.
-   Tests for simultaneous requests against the same sender and for
    opposite-direction transfers (A→B and B→A).
-   Consider `SERIALIZABLE` only if a measured correctness problem
    justifies it.

### NFR-03: Financial consistency

-   Payment record, balance changes, ledger entries, idempotency result,
    audit record, and outbox event are committed in one transaction.
-   Any unexpected failure before commit rolls back everything.
-   Ledger entries and audit records are immutable. Enforced in the
    database by a trigger that rejects `UPDATE`/`DELETE` (and by not
    granting those privileges to the application role). Corrections use
    reversal entries.
-   For every successful transfer, total debit equals total credit in
    the same currency.
-   Account balance is a projection; the ledger is the source of truth.
-   A reconciliation command checks: (a) each account balance equals the
    sum of its ledger entries, and (b) the global ledger sum is zero.
    It runs in tests and can be run on demand.

### NFR-04: Performance and latency

-   Indexes for payment ID, account history, idempotency scope/key, and
    ledger lookups.
-   Connection pooling with `pgxpool` and explicit limits.
-   Avoid N+1 queries and unnecessary round trips.
-   Paginate history endpoints.
-   Measure p50/p95/p99 latency, throughput, and error rate with k6.
-   Set targets only after measuring a baseline.

### NFR-05: Security

-   Authenticate every API request (minimal bearer-token auth from
    Phase 2; hardened in Phase 7).
-   Derive user identity from the authenticated context, never from a
    request field.
-   Authorize: sender ownership for payments; ownership for reads;
    admin role for top-ups.
-   Return `404` (not `403`) for resources the caller cannot see, to
    avoid revealing which IDs exist.
-   Validate amount, currency, account status, and ownership.
-   Never log tokens, secrets, or full request bodies containing them.
-   Parameterized SQL only; safe error responses.
-   Rate limiting in V2.

### NFR-06: Reliability and recoverability

-   Database constraints in addition to application validation.
-   Clear transaction boundaries.
-   Test database errors, process termination, retries, and lock
    contention.
-   Migrations and a reproducible local environment from Phase 0.
-   Graceful shutdown: stop accepting requests, finish in-flight
    transactions, close the pool.
-   Document backup/restore (`pg_dump`/`pg_restore`) for local data.

### NFR-07: Observability

-   Structured JSON logs (`log/slog`) with a request ID on every line.
-   Request ID accepted from `X-Request-ID` or generated, returned in
    the response, and stored in audit records.
-   Basic metrics endpoint (`/metrics`, Prometheus format) in V1;
    dashboards in V2.

------------------------------------------------------------------------

## 4. Proposed architecture

Start with a **modular monolith** written in Go. This keeps deployment
and debugging manageable while still enforcing clear module boundaries.
Do not split into microservices merely to make the diagram look
sophisticated.

### 4.1 High-level components

``` text
React + TypeScript dashboard (V2)
             |
             v
       Go HTTP API  (auth, request ID, logging middleware)
             |
             v
     Payment / Account Services
     /      |        |        \
    v       v        v         v
Accounts Payments Idempotency Audit
     \      |        |        /
      \     v        v       /
            PostgreSQL
             |
             v
     Ledger + Outbox + Audit log
             |
       Go background worker (outbox, idempotency cleanup)
             |
       Optional NATS (V2)

Python AI service (V2)
      Risk/anomaly analysis
      Failure-pattern analysis
      Explanations and insights
```

### 4.2 Technology choices

| Area | Choice | Reason |
|---|---|---|
| Backend | Go (latest stable) | Primary backend language, strong concurrency |
| HTTP routing | `chi` | Small, idiomatic, stdlib-compatible |
| Database | PostgreSQL 16+ | Transactions, constraints, row locks, indexing |
| Database access | `pgx` v5 + `pgxpool` | Explicit SQL and transaction control |
| Migrations | `goose` | Versioned SQL migrations |
| Logging | `log/slog` (stdlib) | Structured JSON logs |
| Config | Environment variables | 12-factor style; `.env` for local only |
| Integration tests | `testcontainers-go` (real PostgreSQL) | No mocks on the money path |
| Containers | Docker Desktop (WSL2) + Docker Compose | Reproducible local environment |
| CI | GitHub Actions | `go vet`, `go test`, migrations check on every push |
| Load testing | k6 | Concurrent load and latency measurement |
| Metrics (V2 dashboards) | Prometheus + Grafana | Local metrics and visualization |
| AI service (V2) | Python + FastAPI, scikit-learn, Pandas | Separate service for ML and analysis |
| Frontend (V2) | React + TypeScript | User dashboard |
| Optional messaging (V2) | NATS JetStream, self-hosted | Event-driven architecture exercise |

No Kafka, Redis, or MinIO. A PostgreSQL outbox and Go worker are
sufficient to learn reliable event handoff.

### 4.3 Free-first infrastructure policy

-   Prefer open-source software and local development.
-   Do not make a paid managed service a prerequisite.
-   Check current free-tier terms before using any hosted service.
-   Keep the project runnable locally through Docker Compose.
-   Do not claim that a local simulation is identical to a managed
    AWS/GCP service; document what it demonstrates and what it does not.

### 4.4 Development environment

-   OS: Windows 11 with WSL2 + Docker Desktop. Go runs natively on
    Windows; PostgreSQL runs only in Docker.
-   Source control: GitHub, repository `fincore`, managed via `git` and
    the GitHub CLI (`gh`).
-   Branching: `main` is protected by CI; work happens on short-lived
    feature branches merged via pull request.

------------------------------------------------------------------------

## 5. Domain model and class responsibilities

The following are conceptual classes. In Go, these become structs,
interfaces, and service/repository packages.

### User

-   `ID` (UUID), `Name`, `Email` (unique), `Role` (`USER` or `ADMIN`),
    `CreatedAt`

### Account

-   `ID` (UUID), `UserID` (nullable only for the system treasury
    account), `Type` (`USER` or `TREASURY`), `Currency`, `Balance`
    (`BIGINT` minor units), `Status` (`ACTIVE`, `FROZEN`, `CLOSED`),
    `CreatedAt`, `UpdatedAt`

Balance is never edited directly; only validated financial operations
change it. A `CHECK` constraint enforces `balance >= 0` for `USER`
accounts.

### Payment

-   `ID`, `Type` (`TRANSFER` or `TOPUP`), `SenderAccountID`,
    `ReceiverAccountID`, `Amount` (minor units), `Currency`, `Status`,
    `FailureReason` (nullable), `InitiatedByUserID`, `CreatedAt`,
    `UpdatedAt`

### LedgerEntry

-   `ID`, `PaymentID`, `AccountID`, `EntryType` (`DEBIT`/`CREDIT`),
    `Amount` (positive minor units), `Currency`, `CreatedAt`

A successful payment creates exactly two entries with the same payment
reference and amount.

### IdempotencyRecord

-   `UserID`, `Key`, `RequestHash`, `PaymentID`, `ResponseStatus`,
    `ResponseBody`, `CreatedAt`, `ExpiresAt`

Primary key / unique constraint on `(UserID, Key)`.

### AuditLog

-   `ID` (bigserial), `OccurredAt`, `ActorUserID` (nullable for
    system/anonymous), `Action`, `ResourceType`, `ResourceID`,
    `Outcome` (`SUCCESS`, `FAILURE`, `DENIED`), `RequestID`,
    `IPAddress`, `Details` (JSONB, no secrets)

### OutboxEvent

-   `ID`, `EventType` (e.g. `payment.succeeded`), `AggregateID`,
    `Payload` (JSONB), `CreatedAt`, `ProcessedAt`, `Attempts`,
    `LastError`

### Services

`PaymentService` coordinates the payment use case:

-   Validate input and business rules.
-   Begin the transaction.
-   Claim the idempotency key (or replay / reject).
-   Lock accounts in a consistent order.
-   Check ownership, status, currency, and funds.
-   Create the payment (`SUCCESS` or `FAILED`).
-   On success: update balances and insert both ledger entries.
-   Write the audit record and outbox event.
-   Persist the idempotency result.
-   Commit or roll back.

`AccountService` handles account creation, reads, and top-ups (top-ups
reuse the same transactional transfer core).

### Repositories

-   `AccountRepository`, `PaymentRepository`, `LedgerRepository`,
    `IdempotencyRepository`, `AuditRepository`, `OutboxRepository`

A transaction manager passes one `pgx.Tx` to all repositories so they
participate in the same database transaction.

### Conceptual class diagram

``` mermaid
classDiagram
direction TB

class User {
  +UUID id
  +String name
  +String email
  +String role
}

class Account {
  +UUID id
  +UUID userId
  +String type
  +String currency
  +Int64 balanceMinor
  +String status
}

class Payment {
  +UUID id
  +String type
  +UUID senderAccountId
  +UUID receiverAccountId
  +Int64 amountMinor
  +String currency
  +String status
  +String failureReason
  +DateTime createdAt
}

class LedgerEntry {
  +UUID id
  +UUID paymentId
  +UUID accountId
  +String entryType
  +Int64 amountMinor
  +String currency
}

class IdempotencyRecord {
  +UUID userId
  +String key
  +String requestHash
  +UUID paymentId
  +Int responseStatus
  +String responseBody
}

class AuditLog {
  +Int64 id
  +UUID actorUserId
  +String action
  +String resourceId
  +String outcome
  +String requestId
}

class OutboxEvent {
  +UUID id
  +String eventType
  +UUID aggregateId
  +DateTime processedAt
}

class PaymentService {
  +ProcessPayment()
  +GetPayment()
  +GetAccountPayments()
}

class AccountService {
  +CreateAccount()
  +GetAccount()
  +TopUp()
}

User "1" --> "*" Account : owns
Account "1" --> "*" LedgerEntry : has
Payment "1" --> "0..2" LedgerEntry : produces
IdempotencyRecord --> Payment : references
Payment ..> AuditLog : audited by
Payment ..> OutboxEvent : emits
PaymentService --> AuditLog : writes
AccountService --> PaymentService : reuses transfer core
```

------------------------------------------------------------------------

## 6. Payment processing workflow

### 6.1 Synchronous flow

1.  Client sends `POST /api/v1/payments` with an `Idempotency-Key`.
2.  Middleware assigns a request ID and authenticates the caller.
3.  Handler validates request shape, parses amount into minor units,
    and computes the canonical request hash. Invalid input → `400` (not
    stored, audited as a validation failure).
4.  `BEGIN` a PostgreSQL transaction.
5.  Claim the key:
    `INSERT INTO idempotency_records (user_id, key, request_hash, ...) ON CONFLICT DO NOTHING`.
    -   Inserted → continue.
    -   Conflict → `ROLLBACK`, read the existing record:
        different hash → `409`; same hash → replay stored response.
6.  Lock sender and receiver rows in one statement ordered by ID.
7.  Check: both exist, both `ACTIVE`, same currency as request, caller
    owns sender, sender ≠ receiver, sender has sufficient funds.
8.  If a business rule fails: insert payment with `FAILED` and
    `failure_reason`; no balance or ledger change; response `422`.
9.  Otherwise: insert payment with `SUCCESS`; debit sender and credit
    receiver; insert DEBIT and CREDIT ledger entries; insert
    `payment.succeeded` outbox event; response `201`.
10. Insert the audit record (success or failure).
11. Update the idempotency record with the response status, body, and
    payment ID.
12. `COMMIT`.
13. Return the response.

Any unexpected error (database error, timeout, panic) before commit
rolls back everything, including the idempotency claim, so the client
can safely retry. Such errors are audited outside the transaction on a
best-effort basis and logged.

### 6.2 Sequence diagram

``` mermaid
sequenceDiagram
    participant Client
    participant API as Go HTTP API
    participant Service as Payment Service
    participant DB as PostgreSQL

    Client->>API: POST /api/v1/payments + Idempotency-Key
    API->>API: Auth, request ID, validate, hash request
    API->>Service: ProcessPayment(request, identity)
    Service->>DB: BEGIN
    Service->>DB: INSERT idempotency claim ON CONFLICT DO NOTHING

    alt Key already exists
        Service->>DB: ROLLBACK
        Service->>DB: Read existing record
        alt Same request hash
            Service-->>API: Stored response
            API-->>Client: Replay (original status) + Idempotent-Replayed: true
        else Different hash
            API-->>Client: 409 Conflict
        end
    else New key
        Service->>DB: SELECT accounts ORDER BY id FOR UPDATE
        Service->>DB: Validate ownership, status, currency, funds
        alt Business rule fails
            Service->>DB: Insert FAILED payment
        else Valid
            Service->>DB: Insert SUCCESS payment
            Service->>DB: Debit sender, credit receiver
            Service->>DB: Insert DEBIT + CREDIT ledger entries
            Service->>DB: Insert outbox event
        end
        Service->>DB: Insert audit record
        Service->>DB: Save idempotency response
        Service->>DB: COMMIT
        Service-->>API: Result
        API-->>Client: 201 Created or 422
    end
```

### 6.3 Why the claim is inside the transaction

When two requests with the same key arrive together, both run the
`INSERT ... ON CONFLICT DO NOTHING`. PostgreSQL makes the second insert
**wait** on the unique index until the first transaction finishes:

-   If the first commits, the second sees the conflict and replays the
    committed result.
-   If the first rolls back, the second's insert succeeds and it
    processes the payment.

Only one request can ever perform the financial mutation, and there is
no stale "in progress" record to clean up after a crash.

### 6.4 Failure persistence rule

| Failure kind | Example | Stored? | Response | Retry behaviour |
|---|---|---|---|---|
| Validation | Malformed JSON, 3 decimal places | No | `400` | Re-evaluated |
| Business rule | Insufficient funds, inactive account | Yes, `FAILED` payment | `422` | Replays `422` |
| Idempotency conflict | Same key, different body | No change | `409` | `409` |
| Lock/statement timeout | Contention | No (rolled back) | `503` | Re-evaluated |
| Unexpected | DB connection lost | No (rolled back) | `500` | Re-evaluated |

------------------------------------------------------------------------

## 7. API design

| Method | Endpoint | Purpose | Who |
|---|---|---|---|
| `POST` | `/api/v1/accounts` | Create an account | User |
| `GET` | `/api/v1/accounts/{account_id}` | Account details and balance | Owner |
| `POST` | `/api/v1/accounts/{account_id}/topups` | Fund an account from treasury | Admin |
| `POST` | `/api/v1/payments` | Initiate an internal transfer | Sender owner |
| `GET` | `/api/v1/payments/{payment_id}` | Payment details | Sender or receiver owner |
| `GET` | `/api/v1/accounts/{account_id}/payments` | Payment history (cursor) | Owner |
| `GET` | `/healthz` | Liveness | Public |
| `GET` | `/readyz` | Readiness (DB reachable) | Public |
| `GET` | `/metrics` | Prometheus metrics | Internal |

### Successful payment response

``` json
{
  "payment_id": "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b",
  "type": "TRANSFER",
  "sender_account_id": "0b8f6a52-1c3e-4d7a-9f2b-5e6c7d8a9b01",
  "receiver_account_id": "7c2d1e4f-8a9b-4c3d-b2e1-0f9e8d7c6b5a",
  "amount": "2000.00",
  "currency": "INR",
  "status": "SUCCESS",
  "created_at": "2026-01-01T10:00:00Z"
}
```

Values are illustrative.

### Error response format

``` json
{
  "error": {
    "code": "INSUFFICIENT_FUNDS",
    "message": "Sender account has insufficient funds.",
    "request_id": "req_01HZX..."
  }
}
```

### HTTP outcomes

-   `201 Created`: new payment/account/top-up committed.
-   Replay: original status code and body, plus
    `Idempotent-Replayed: true`.
-   `400 Bad Request`: malformed or invalid input, missing
    `Idempotency-Key`.
-   `401 Unauthorized`: missing or invalid authentication.
-   `403 Forbidden`: authenticated but lacks the role (e.g. non-admin
    top-up).
-   `404 Not Found`: resource does not exist **or** caller may not see
    it.
-   `409 Conflict`: idempotency key reused with a different request.
-   `422 Unprocessable Content`: business rule failure.
-   `503 Service Unavailable`: lock/statement timeout; safe to retry.
-   `500 Internal Server Error`: unexpected failure, no internal
    details.

Every status code above has at least one automated test.

------------------------------------------------------------------------

## 8. Database design

The detailed ER diagram and SQL migrations are produced in Phase 1.

Tables:

-   `users`
-   `accounts`
-   `payments`
-   `ledger_entries`
-   `idempotency_records`
-   `audit_log`
-   `outbox_events`

### 8.1 Constraints

-   UUID primary keys (`audit_log` uses `BIGSERIAL` for ordering).
-   Foreign keys: accounts→users, payments→accounts,
    ledger_entries→payments/accounts, idempotency_records→payments.
-   `amount > 0` on payments and ledger entries.
-   `sender_account_id <> receiver_account_id`.
-   `currency` explicit, `CHECK (currency IN ('INR'))` in V1.
-   `balance >= 0` for `USER` accounts.
-   Exactly one `TREASURY` account per currency (partial unique index).
-   Unique `(user_id, key)` on idempotency records.
-   `CHECK` constraints for all status/type enums.
-   Triggers rejecting `UPDATE`/`DELETE` on `ledger_entries` and
    `audit_log`.

Cross-row invariants (exactly two balanced entries per successful
payment; balance = ledger sum) are enforced in the transaction and
verified by the reconciliation command and tests.

### 8.2 Indexes

-   `payments(id)` (PK)
-   `ledger_entries(account_id, created_at DESC, id DESC)` --- account
    history uses ledger entries joined to payments, so one index covers
    both sent and received payments. Failed payments (no ledger entries)
    are found via `payments(sender_account_id, created_at DESC, id DESC)`.
-   `ledger_entries(payment_id)`
-   `idempotency_records(expires_at)` for cleanup
-   `audit_log(actor_user_id, occurred_at)`,
    `audit_log(resource_type, resource_id)`
-   `outbox_events(created_at) WHERE processed_at IS NULL`

### 8.3 Audit log

The audit log answers "who did what, when, from where, and what
happened" --- separate from the ledger, which answers "where did the
money go".

**Audited actions (V1):**

| Action | When | Same transaction as change? |
|---|---|---|
| `payment.create` | Success and business failure | Yes |
| `topup.create` | Success and failure | Yes |
| `account.create` | Success | Yes |
| `idempotency.conflict` | `409` returned | No (best effort) |
| `auth.failure` | Invalid/missing token | No (best effort) |
| `authz.denied` | Access to another user's resource | No (best effort) |
| `request.validation_failed` | `400` on money endpoints | No (best effort) |
| `reconciliation.run` | Reconciliation executed, with result | Yes |

**Rules:**

-   Append-only; `UPDATE`/`DELETE` rejected by trigger.
-   Every record carries the `request_id`, so logs, audit records, and
    payments can be correlated.
-   `details` JSONB never contains tokens, secrets, or full auth
    headers.
-   Money-moving audit records commit atomically with the money change;
    if the audit insert fails, the payment rolls back.
-   Retention: kept indefinitely in V1 (simulation data volume is
    small).
-   Read access via an admin-only query/CLI in V1; an admin API endpoint
    may be added in V2.

### 8.4 Ledger and balance model

The account balance is a projection for fast reads. Ledger entries are
the immutable source of truth. Every balance change has matching ledger
entries in the same transaction.

INR 2,000 transfer:

| Account | Entry | Amount (paise) |
|---|---|---|
| Sender | DEBIT | 200000 |
| Receiver | CREDIT | 200000 |

INR 5,000 top-up:

| Account | Entry | Amount (paise) |
|---|---|---|
| Treasury | DEBIT | 500000 |
| User | CREDIT | 500000 |

Debits equal credits per payment; the global ledger sum is zero.

### 8.5 Outbox

-   Event in V1: `payment.succeeded` (and `topup.succeeded`).
-   Worker polls with `SELECT ... FOR UPDATE SKIP LOCKED`, processes,
    sets `processed_at`. Retries with backoff, records `attempts` and
    `last_error`.
-   V1 consumer: writes a row to a `notifications` table (simulated
    "you received money" message). Delivery is at-least-once, so the
    consumer must be idempotent (unique on event ID).
-   Same worker process runs the idempotency-record cleanup job.

------------------------------------------------------------------------

## 9. AI intelligence service --- V2

AI is an extension to the payment platform, not a dependency for moving
money.

The AI service is a separate Python application using FastAPI. It uses
synthetic payment data first, then read-only access to FinCore data
(e.g. a read replica role or exported data).

Potential capabilities:

1.  **Anomaly detection:** flag unusual transaction patterns (e.g.
    Isolation Forest).
2.  **Failure analysis:** group `FAILED` payments by `failure_reason`,
    time window, and account features.
3.  **Risk scoring:** advisory risk score for investigation.
4.  **Natural-language analytics:** translate a constrained question
    into an approved, pre-defined analytics query.

### AI safety boundary

-   The Go payment service remains authoritative for payment state and
    balances.
-   AI must not independently debit, credit, approve, reject, or refund
    money.
-   The AI service's database role is read-only.
-   Financial rules are deterministic and tested.
-   AI output is advisory, explainable, and recorded in the audit log
    when shown to a user.
-   Use synthetic or anonymized data for experiments.
-   External paid LLM APIs are optional.

------------------------------------------------------------------------

## 10. Testing strategy

Testing starts in Phase 2 and grows with every feature. Integration and
concurrency tests run against a real PostgreSQL (testcontainers-go); the
money path is never mocked. CI runs the full suite on every push.

### Unit tests

-   Amount parsing (`"2000.00"` → 200000; reject `"1.001"`, `"-5"`,
    `"0"`, `"abc"`, overflow).
-   Currency validation.
-   Business rules and status values.
-   Canonical request hashing (field order, whitespace independent).
-   Ledger entry construction.
-   Cursor encoding/decoding.

### Integration tests

-   Successful transfer and top-up.
-   Insufficient balance → `FAILED` stored, balances unchanged.
-   Inactive sender or receiver.
-   Sender not owned by caller → `404`.
-   Receiver owned by someone else → allowed.
-   Currency mismatch; sender = receiver.
-   Rollback after an injected failure (no partial rows anywhere).
-   Audit record written for each audited action; audit and ledger
    `UPDATE`/`DELETE` rejected.
-   Payment and history retrieval; pagination stability; authorization.

### Concurrency tests

-   Many parallel payments from one sender: final balance never
    negative; succeeded count × amount = balance drop.
-   Many transfers into the same receiver.
-   Opposite-direction transfers (A→B, B→A) without deadlock.
-   Same idempotency key sent concurrently: exactly one payment.
-   Same key with different payloads → one succeeds, others `409`.
-   After every concurrency test: reconciliation passes (balances match
    ledger; global sum zero).

### Failure tests

-   Database connection lost before commit.
-   Process killed during a transaction.
-   Client times out after commit and retries → replay, no double
    charge.
-   Worker crashes while processing an outbox event.
-   Duplicate event delivery → consumer stays idempotent.
-   Lock timeout → `503`, nothing committed.

### Performance tests

Use k6. Record requests per second, p50/p95/p99 latency, error rate,
connection pool usage, lock waits, and query timings. Record hardware,
configuration, dataset size, and results in `docs/performance/`. No
invented performance claims.

------------------------------------------------------------------------

## 11. Deployment and operations --- free-first approach

### Local development

Docker Compose runs PostgreSQL (from Phase 0), the Go API and worker
(from Phase 2), and in V2 the Python service and monitoring stack.

### Production-style practices

-   Multi-stage Docker builds.
-   Environment-based configuration; `.env.example` committed, `.env`
    git-ignored.
-   Secrets kept outside source control.
-   Migrations run as a release step.
-   Health and readiness endpoints.
-   Structured logs with request IDs.
-   Metrics (V1 endpoint, V2 dashboards).
-   Graceful shutdown.
-   CI on every push and pull request.
-   Reproducible setup instructions in the README.

### Local substitutes for cloud concepts

| Cloud concept | Local learning implementation |
|---|---|
| Compute instance | Docker container |
| Managed PostgreSQL | PostgreSQL in Docker |
| Load balancer / reverse proxy | Caddy (V2, optional) |
| Queue/event transport | PostgreSQL outbox; NATS in V2 |
| Metrics | Prometheus (V2) |
| Dashboards | Grafana (V2) |
| CI/CD | GitHub Actions free allowance |
| Container deployment | Docker Compose |

A hosted deployment is optional and must be checked for current costs
and free-tier limits before use.

------------------------------------------------------------------------

## 12. Learning and implementation roadmap

Understanding and correctness take priority over dates.

### V1

| Phase | Focus | Deliverable |
|---|---|---|
| 0 | Git, GitHub, terminal, HTTP, Go setup, WSL2 + Docker | Repo on GitHub, Go module, Compose with PostgreSQL, CI running `go vet`/`go test`, README |
| 1 | PostgreSQL, SQL, schema design, ACID, indexes, constraints | ER diagram, goose migrations for all V1 tables incl. audit log and triggers |
| 2 | Go API, minimal auth, accounts, top-ups, payments, ledger, audit log, structured logging | Working API with unit + integration tests from day one |
| 3 | Idempotency and concurrency | In-transaction claim, row locking, concurrency tests, reconciliation command |
| 4 | Failure injection and test hardening | Failure tests; every status code covered |
| 5 | Outbox and background worker | Reliable event handoff, notifications consumer, idempotency cleanup |
| 6 | Docker image, metrics endpoint, k6 load test, docs | Multi-stage build, baseline performance report, V1 release |

### V2

| Phase | Focus | Deliverable |
|---|---|---|
| 7 | Hardened auth (JWT, password hashing), rate limiting | Secured APIs |
| 8 | Prometheus + Grafana | Dashboards and alerts |
| 9 | Python fundamentals and FastAPI | Separate Python service |
| 10 | Data analysis and anomaly detection | First ML capability |
| 11 | React + TypeScript | Working dashboard |
| 12 | Optional NATS, optional hosted deployment | Event-driven experiment, deployment write-up |

------------------------------------------------------------------------

## 13. Repository structure

``` text
fincore/
├── .github/
│   └── workflows/
│       └── ci.yml
├── backend/
│   ├── cmd/
│   │   ├── api/
│   │   ├── worker/
│   │   └── fincorectl/        # admin CLI: seed users, reconcile, read audit log
│   ├── internal/
│   │   ├── auth/
│   │   ├── accounts/
│   │   ├── payments/
│   │   ├── ledger/
│   │   ├── idempotency/
│   │   ├── audit/
│   │   ├── outbox/
│   │   ├── money/              # amount parsing and minor units
│   │   ├── platform/           # db pool, tx manager, config, logging
│   │   └── httpapi/            # router, middleware, handlers, errors
│   ├── migrations/
│   └── tests/                  # integration and concurrency tests
├── ai-service/                 # V2
├── frontend/                   # V2
├── infrastructure/
│   ├── docker/
│   └── monitoring/             # V2
├── docs/
│   ├── architecture/
│   ├── api/
│   ├── database/
│   ├── decisions/              # ADRs: one file per significant decision
│   └── performance/
├── scripts/
├── .env.example
├── docker-compose.yml
└── README.md
```

Create folders only when the corresponding phase begins.

------------------------------------------------------------------------

## 14. Working with Claude Code

Claude Code may assist with learning, implementation, refactoring,
tests, and debugging. It must not replace understanding.

For every significant feature:

1.  **Learn:** explain the underlying concept and terminology.
2.  **Design:** agree on behaviour, interfaces, data flow, and failure
    cases.
3.  **Implement:** write a small, reviewable change on a feature branch.
4.  **Inspect:** read the code and understand important decisions.
5.  **Test:** run unit and integration tests locally and in CI.
6.  **Break:** introduce realistic failure scenarios.
7.  **Debug:** identify the root cause and verify the fix.
8.  **Explain:** record significant decisions as ADRs in
    `docs/decisions/` and explain them without relying on the assistant.

Git workflow: Claude Code commits and pushes using `git` and the GitHub
CLI (`gh`, already authenticated); pull requests are opened with `gh pr
create` and merged after CI passes and the developer has reviewed the
change.

Do not accept generated code that you cannot explain or test.

------------------------------------------------------------------------

## 15. Definition of done (V1)

-   A valid internal transfer debits the sender and credits the
    receiver.
-   Both ledger entries are persisted atomically.
-   Accounts can be created and funded only via the audited top-up path.
-   An insufficient-balance transfer is stored as `FAILED` without
    changing either account, and its retry replays the same `422`.
-   A duplicate request does not create a second transfer.
-   Reusing an idempotency key with a different payload returns `409`.
-   Concurrent transfers preserve account and ledger correctness;
    reconciliation passes after every concurrency test.
-   Every audited action produces an immutable audit record linked by
    request ID.
-   Payment details and account history are retrievable only by
    authorized users.
-   Automated tests cover normal, invalid, duplicate, concurrent, and
    failure paths, and run in CI.
-   The project starts locally with documented instructions
    (`docker compose up`).
-   Logs, audit records, and basic metrics make failures diagnosable.
-   Performance results are measured and documented.
-   The developer can explain the transaction boundary, locking,
    idempotency, ledger, audit log, and failure behaviour.

------------------------------------------------------------------------

## 16. Confirmed decisions

| # | Decision | Confirmed choice |
|---|---|---|
| 1 | Account model | Internal FinCore wallet accounts plus one system treasury account per currency |
| 2 | Insufficient funds | Reject; store as `FAILED`; user balances never negative |
| 3 | Processing model | Synchronous for V1; `PROCESSING` reserved for later |
| 4 | Currency and precision | INR only; `BIGINT` paise; API decimal strings with ≤ 2 places |
| 5 | Identity model | Individual users with `USER`/`ADMIN` roles; no merchants/tenants in V1 |
| 6 | Idempotency | Scope `(user_id, key)`; claim inside the transaction; 24 h retention; replay original status + `Idempotent-Replayed` header |
| 7 | Not-visible resources | `404`, not `403` |
| 8 | Audit log | Mandatory in V1; append-only table; money actions audited in the same transaction |
| 9 | Auth | Minimal bearer tokens from Phase 2; hardened JWT in V2 |
| 10 | Tooling | chi, pgx v5, goose, slog, testcontainers-go, Docker Desktop on WSL2, GitHub Actions |

Changes to these decisions are recorded as ADRs in `docs/decisions/`.

------------------------------------------------------------------------

## 17. Guiding principles

-   Correctness before performance tuning.
-   Financial integrity before feature count.
-   Simple architecture before distributed complexity.
-   Every money movement has a ledger entry and an audit record.
-   Measure latency instead of guessing.
-   Use open-source and free-first tooling.
-   Never use floating-point arithmetic for money.
-   Never allow AI to be the authority for financial mutations.
-   Document trade-offs and operational limitations honestly.
-   Learn each layer from fundamentals to implementation and deployment.

**FinCore's success is measured by how well it is engineered, tested,
explained, and operated---not by how many technologies appear in its
README.**
