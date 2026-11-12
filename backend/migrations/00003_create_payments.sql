-- +goose Up
CREATE TABLE payments (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    type                 text        NOT NULL CHECK (type IN ('TRANSFER', 'TOPUP')),
    sender_account_id    uuid        NOT NULL REFERENCES accounts (id),
    receiver_account_id  uuid        NOT NULL REFERENCES accounts (id),
    amount               bigint      NOT NULL CHECK (amount > 0),
    currency             char(3)     NOT NULL CHECK (currency = 'INR'),
    status               text        NOT NULL CHECK (status IN ('PROCESSING', 'SUCCESS', 'FAILED')),
    failure_reason       text,
    initiated_by_user_id uuid        NOT NULL REFERENCES users (id),
    request_id           text        NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payments_distinct_accounts_check
        CHECK (sender_account_id <> receiver_account_id),
    -- A FAILED payment always has a reason; any other status never has one.
    CONSTRAINT payments_failure_reason_check
        CHECK ((status = 'FAILED') = (failure_reason IS NOT NULL))
);

-- Account history (FR-07): one keyset index per side, merged with UNION ALL.
CREATE INDEX payments_sender_history_idx
    ON payments (sender_account_id, created_at DESC, id DESC);
CREATE INDEX payments_receiver_history_idx
    ON payments (receiver_account_id, created_at DESC, id DESC);

-- +goose Down
DROP TABLE payments;
