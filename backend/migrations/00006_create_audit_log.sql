-- +goose Up

-- No foreign keys by design: the audit log must accept records about
-- resources that do not exist (e.g. denied requests) and must never be
-- blocked by, or block, changes to other tables.
CREATE TABLE audit_log (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    actor_user_id uuid,
    action        text        NOT NULL,
    resource_type text,
    resource_id   text,
    outcome       text        NOT NULL CHECK (outcome IN ('SUCCESS', 'FAILURE', 'DENIED')),
    request_id    text,
    ip_address    inet,
    details       jsonb       NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_log_actor_idx    ON audit_log (actor_user_id, occurred_at);
CREATE INDEX audit_log_resource_idx ON audit_log (resource_type, resource_id);
CREATE INDEX audit_log_request_idx  ON audit_log (request_id);

CREATE TRIGGER audit_log_no_update_delete
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION fincore_reject_modification();

CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION fincore_reject_modification();

-- +goose Down
DROP TABLE audit_log;
