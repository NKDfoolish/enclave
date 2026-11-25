-- Baseline migration. Real tables (workspaces, users, memberships) arrive in Part 1.

-- +goose Up
-- Case-insensitive text, used later for email addresses.
CREATE EXTENSION IF NOT EXISTS citext;

-- +goose Down
DROP EXTENSION IF EXISTS citext;
