-- +goose Up
-- The single INR treasury account: the double-entry source of all money
-- issued into FinCore via top-ups (Blueprint FR-10).
INSERT INTO accounts (type, currency) VALUES ('TREASURY', 'INR');

-- +goose Down
-- Refuse to roll back once money has moved: removing the treasury would
-- destroy financial history. Rolling back is only for empty development
-- databases; anything else should be restored from a backup instead.
-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n
    FROM payments p
    JOIN accounts a ON a.id IN (p.sender_account_id, p.receiver_account_id)
    WHERE a.type = 'TREASURY' AND a.currency = 'INR';
    IF n > 0 THEN
        RAISE EXCEPTION 'refusing to remove the INR treasury: % payment(s) reference it; rolling back would destroy financial history (restore from a backup instead)', n
            USING ERRCODE = 'restrict_violation';
    END IF;
END
$$;
-- +goose StatementEnd
DELETE FROM accounts WHERE type = 'TREASURY' AND currency = 'INR';
