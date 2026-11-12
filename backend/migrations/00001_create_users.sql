-- +goose Up
CREATE TABLE users (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    email          text        NOT NULL CHECK (char_length(email) BETWEEN 3 AND 320),
    role           text        NOT NULL DEFAULT 'USER' CHECK (role IN ('USER', 'ADMIN')),
    -- SHA-256 of the bearer token; the raw token is never stored.
    api_token_hash bytea       NOT NULL CHECK (octet_length(api_token_hash) = 32),
    created_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_api_token_hash_key UNIQUE (api_token_hash)
);

CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));

-- +goose Down
DROP TABLE users;
