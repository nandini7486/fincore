-- +goose Up

-- fincore_app is a NOLOGIN group role holding the API/worker privileges.
-- The login user the application connects as is created per environment
-- (outside migrations, so no password lives in source control) and is
-- granted membership: GRANT fincore_app TO <login_user>;
-- Roles are cluster-wide, so creation is idempotent.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fincore_app') THEN
        CREATE ROLE fincore_app NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO fincore_app;

GRANT SELECT, INSERT, UPDATE
    ON users, accounts, payments, idempotency_records, outbox_events, notifications
    TO fincore_app;

-- Expired idempotency records are removed by the cleanup job.
GRANT DELETE ON idempotency_records TO fincore_app;

-- Append-only tables: read and insert, never update or delete.
GRANT SELECT, INSERT ON ledger_entries, audit_log TO fincore_app;

-- +goose Down
-- The role itself is kept: it is cluster-wide and may be used by other databases.
REVOKE ALL ON users, accounts, payments, idempotency_records, outbox_events,
              notifications, ledger_entries, audit_log
    FROM fincore_app;
REVOKE USAGE ON SCHEMA public FROM fincore_app;
