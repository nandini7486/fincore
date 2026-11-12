-- +goose Up
CREATE TABLE accounts (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        REFERENCES users (id),
    type       text        NOT NULL CHECK (type IN ('USER', 'TREASURY')),
    currency   char(3)     NOT NULL CHECK (currency = 'INR'),
    -- Minor units (paise). A projection of the ledger, never edited directly.
    balance    bigint      NOT NULL DEFAULT 0,
    status     text        NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'FROZEN', 'CLOSED')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- User accounts have an owner and never go negative.
    -- The treasury has no owner and may go negative (it issues money).
    CONSTRAINT accounts_owner_balance_check CHECK (
        (type = 'USER'     AND user_id IS NOT NULL AND balance >= 0) OR
        (type = 'TREASURY' AND user_id IS NULL)
    )
);

CREATE UNIQUE INDEX accounts_one_treasury_per_currency
    ON accounts (currency) WHERE type = 'TREASURY';

CREATE INDEX accounts_user_id_idx ON accounts (user_id);

-- +goose Down
DROP TABLE accounts;
