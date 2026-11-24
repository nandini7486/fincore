# FinCore — Frontend

React + TypeScript dashboard. **Planned for V2 (Phase 11)** — see the
[blueprint roadmap](../docs/FINCORE_PROJECT_BLUEPRINT.md#12-learning-and-implementation-roadmap).

Planned scope:

- Sign in with an API token (JWT after Phase 7)
- Accounts and balances
- Send money (with an idempotency key generated per attempt)
- Payment history with cursor pagination
- Live "money received" notifications via Server-Sent Events

The frontend only talks to the public API under `/api/v1`. It never reads
the database directly and holds no financial logic.
