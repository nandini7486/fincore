# 0001: Return 404 for every account the caller may not see

- **Status:** Accepted
- **Date:** 2026-11-24
- **Phase:** 2b (accounts)

## Context

`GET /api/v1/accounts/{account_id}` is owner-only (FR-09). A caller can ask for
an ID that is malformed, does not exist, belongs to another user, or is the
system treasury.

If these cases get different answers (`400`, `404`, `403`), the API becomes an
oracle: an attacker can tell which account IDs exist and which belong to
someone. That is the first step of **broken object level authorization
(BOLA / IDOR)**, the top risk in the OWASP API Security Top 10, and a common
cause of real fintech data leaks.

## Decision

- All four cases return the same `404` with code `ACCOUNT_NOT_FOUND` and the
  same message (NFR-05).
- Malformed IDs are rejected before querying, so PostgreSQL never sees them.
- When the account **exists but belongs to someone else** (including the
  treasury), an `authz.denied` audit record is written on a best-effort basis.
  Missing and malformed IDs are not audited: they reveal nothing and would only
  add noise.
- Ownership is checked against the identity from the bearer token, never a
  request field.

## Consequences

- Clients cannot distinguish "not yours" from "does not exist". The owner gets
  the account, everyone else gets the same 404.
- Security reviewers can still see probing attempts in the audit log.
- Known limitation: the "exists but not yours" path does an extra audit
  insert, so its timing differs slightly. Acceptable for V1; if needed later,
  the audit write can be moved off the request path (e.g. via the outbox).
