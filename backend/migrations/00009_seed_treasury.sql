-- +goose Up
-- The single INR treasury account: the double-entry source of all money
-- issued into FinCore via top-ups (Blueprint FR-10).
INSERT INTO accounts (type, currency) VALUES ('TREASURY', 'INR');

-- +goose Down
DELETE FROM accounts WHERE type = 'TREASURY' AND currency = 'INR';
