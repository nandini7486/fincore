-- +goose Up
CREATE TABLE outbox_events (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type      text        NOT NULL,
    aggregate_id    uuid        NOT NULL,
    payload         jsonb       NOT NULL,
    attempts        int         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error      text,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    processed_at    timestamptz
);

-- The worker only ever scans pending events.
CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at) WHERE processed_at IS NULL;

CREATE TABLE notifications (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id),
    -- One notification per outbox event: makes the consumer idempotent.
    event_id   uuid        NOT NULL,
    message    text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT notifications_event_id_key UNIQUE (event_id)
);

CREATE INDEX notifications_user_idx ON notifications (user_id, created_at);

-- +goose Down
DROP TABLE notifications;
DROP TABLE outbox_events;
