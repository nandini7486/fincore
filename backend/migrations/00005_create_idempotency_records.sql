-- +goose Up
CREATE TABLE idempotency_records (
    user_id         uuid        NOT NULL REFERENCES users (id),
    key             text        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
    -- SHA-256 of operation + canonical request body.
    request_hash    bytea       NOT NULL CHECK (octet_length(request_hash) = 32),
    -- Response columns are NULL only inside the claiming transaction.
    payment_id      uuid        REFERENCES payments (id),
    response_status int,
    response_body   jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL DEFAULT now() + interval '24 hours',

    -- This key is what makes the in-transaction claim safe (Blueprint §6.3).
    PRIMARY KEY (user_id, key)
);

CREATE INDEX idempotency_records_expires_at_idx ON idempotency_records (expires_at);

-- +goose Down
DROP TABLE idempotency_records;
