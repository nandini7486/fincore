-- +goose Up

-- Shared by every append-only table (ledger_entries, audit_log).
-- +goose StatementBegin
CREATE FUNCTION fincore_reject_modification() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: table is append-only', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TABLE ledger_entries (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id uuid        NOT NULL REFERENCES payments (id),
    account_id uuid        NOT NULL REFERENCES accounts (id),
    entry_type text        NOT NULL CHECK (entry_type IN ('DEBIT', 'CREDIT')),
    amount     bigint      NOT NULL CHECK (amount > 0),
    currency   char(3)     NOT NULL CHECK (currency = 'INR'),
    created_at timestamptz NOT NULL DEFAULT now(),

    -- At most one DEBIT and one CREDIT per payment.
    CONSTRAINT ledger_entries_one_per_side_key UNIQUE (payment_id, entry_type)
);

CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, created_at, id);

CREATE TRIGGER ledger_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION fincore_reject_modification();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION fincore_reject_modification();

-- +goose Down
DROP TABLE ledger_entries;
DROP FUNCTION fincore_reject_modification();
