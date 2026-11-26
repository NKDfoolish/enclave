-- RAG and Q&A (Part 4): the search index's state per document, and private conversations.
-- The RAG service owns document_index (it writes it after indexing; the Core API reads it to
-- show progress). The Core API owns conversations and conversation_messages.

-- +goose Up

-- One row per document the RAG service has indexed or failed to index. No row for a ready
-- document means indexing is still pending. Deleting the document deletes its row.
CREATE TABLE document_index (
    document_id     uuid PRIMARY KEY REFERENCES documents (id) ON DELETE CASCADE,
    status          text NOT NULL CHECK (status IN ('indexed', 'failed')),
    -- The gateway alias that produced the vectors. A different configured model means the
    -- document must be indexed again: vectors from two models cannot be compared.
    embedding_model text NOT NULL,
    -- SHA-256 of the file that was indexed; an unchanged file with the same model is not
    -- embedded again.
    content_sha256  bytea,
    chunk_count     integer NOT NULL DEFAULT 0 CHECK (chunk_count >= 0),
    error           text,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- A conversation belongs to one user in one workspace and is visible to that user only.
CREATE TABLE conversations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title        text NOT NULL DEFAULT '' CHECK (length(title) <= 200),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX conversations_owner_idx ON conversations (workspace_id, user_id, updated_at DESC);

CREATE TABLE conversation_messages (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    role            text NOT NULL CHECK (role IN ('user', 'assistant')),
    content         text NOT NULL,
    -- For answers: the sources cited, [{number, document_id, title, page, section, snippet}].
    -- Kept so an answer can be hidden later if the user loses access to one of its sources.
    citations       jsonb NOT NULL DEFAULT '[]',
    -- False when the answer cites no source ("not enough information").
    grounded        boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX conversation_messages_conversation_idx ON conversation_messages (conversation_id, id);

-- +goose Down

DROP TABLE conversation_messages;
DROP TABLE conversations;
DROP TABLE document_index;
