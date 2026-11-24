-- +goose Up
-- GET /api/v1/notifications pages newest first on (created_at, id), like
-- payment history (ADR 0002). This index serves that order exactly, so each
-- page is an index range scan with no sort. It replaces the Phase 1 index on
-- (user_id, created_at), which lacked the id tie-breaker.
CREATE INDEX notifications_user_keyset_idx
    ON notifications (user_id, created_at DESC, id DESC);
DROP INDEX notifications_user_idx;

-- +goose Down
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at);
DROP INDEX notifications_user_keyset_idx;
