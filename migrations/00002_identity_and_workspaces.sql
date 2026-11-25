-- Identity (users) and workspace membership (workspaces, memberships, invitations).
-- Login state lives in signed JWTs plus a Redis revocation list, not in this database (ADR 0002).

-- +goose Up

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext NOT NULL UNIQUE,
    display_name  text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 100),
    -- argon2id hash in PHC string format; the plain password is never stored.
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workspaces (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Roles belong to a workspace, not to a user globally.
CREATE TABLE memberships (
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role         text NOT NULL CHECK (role IN ('admin', 'staff', 'client')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);

-- "Which workspaces does this user belong to?" is asked on every login.
CREATE INDEX memberships_user_id_idx ON memberships (user_id);

CREATE TABLE invitations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    email        citext NOT NULL,
    role         text NOT NULL CHECK (role IN ('admin', 'staff', 'client')),
    -- The invite link carries a random token; the database keeps only its SHA-256 hash.
    token_hash   bytea NOT NULL UNIQUE,
    invited_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    accepted_at  timestamptz,
    revoked_at   timestamptz
);

-- At most one open invitation per email per workspace.
CREATE UNIQUE INDEX invitations_open_email_idx
    ON invitations (workspace_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- +goose Down

DROP TABLE invitations;
DROP TABLE memberships;
DROP TABLE workspaces;
DROP TABLE users;
