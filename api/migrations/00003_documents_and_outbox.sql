-- Document library (Part 2) and the transactional outbox used for background work.

-- +goose Up

CREATE TABLE documents (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    title          text NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    -- The name the user uploaded, shown and used for downloads only; never part of a storage path.
    filename       text NOT NULL CHECK (length(filename) BETWEEN 1 AND 255),
    content_type   text NOT NULL CHECK (content_type IN (
                       'application/pdf',
                       'text/markdown',
                       'application/vnd.openxmlformats-officedocument.wordprocessingml.document')),
    size_bytes     bigint NOT NULL CHECK (size_bytes > 0),
    -- SHA-256 of the stored bytes, set once validation has read the file.
    sha256         bytea,
    storage_key    text NOT NULL UNIQUE,
    -- New documents are internal; staff or admins explicitly share them with clients.
    visibility     text NOT NULL DEFAULT 'internal' CHECK (visibility IN ('internal', 'client_common')),
    status         text NOT NULL DEFAULT 'uploading' CHECK (status IN ('uploading', 'processing', 'ready', 'failed')),
    failure_reason text,
    uploaded_by    uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Keyset pagination: newest first within a workspace.
CREATE INDEX documents_workspace_page_idx ON documents (workspace_id, created_at DESC, id DESC);
-- The abandoned-upload sweep looks for old rows still in 'uploading'.
CREATE INDEX documents_uploading_idx ON documents (created_at) WHERE status = 'uploading';

-- Work that must happen after a transaction commits (deleting a stored file, validating an
-- upload). Writing the job in the same transaction as the change that needs it means the
-- job can never be lost, even if the process crashes right after commit.
CREATE TABLE outbox_jobs (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       text NOT NULL,
    payload    jsonb NOT NULL,
    attempts   integer NOT NULL DEFAULT 0,
    run_after  timestamptz NOT NULL DEFAULT now(),
    -- Lease: a worker that claims a job writes its token and a deadline here, then commits and
    -- runs the handler outside any transaction. An expired lease means the worker died or
    -- stalled, and another worker may take the job.
    locked_until timestamptz,
    lease_token  uuid,
    last_error text,
    -- Set when a job keeps failing; it then waits for a human instead of retrying forever.
    dead_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX outbox_jobs_due_idx ON outbox_jobs (run_after) WHERE dead_at IS NULL;

-- +goose Down

DROP TABLE outbox_jobs;
DROP TABLE documents;
