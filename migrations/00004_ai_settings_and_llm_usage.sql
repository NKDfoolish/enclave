-- LLM Gateway (Part 3): which AI providers a workspace allows, and what every model call cost.
-- The Core API owns workspace_ai_settings (admins edit it); the gateway only reads it.
-- The gateway owns llm_usage.

-- +goose Up

-- External providers (hosted outside Enclave, e.g. NVIDIA, Anthropic) that a workspace admin
-- has allowed to receive the workspace's content. No row, or an empty list, means none: the
-- gateway then only uses providers that run inside Enclave, such as a local Ollama.
CREATE TABLE workspace_ai_settings (
    workspace_id       uuid PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    external_providers text[] NOT NULL DEFAULT '{}',
    updated_by         uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- One row per provider call. A request that falls back to a second provider writes two rows,
-- so failed and retried calls are accounted for, not only the one that answered.
CREATE TABLE llm_usage (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The internal service that called the gateway (rag, agent, ...).
    caller         text NOT NULL,
    request_id     text NOT NULL,
    kind           text NOT NULL CHECK (kind IN ('chat', 'embedding')),
    model_alias    text NOT NULL,
    provider       text NOT NULL,
    provider_model text NOT NULL,
    -- 1 for the first provider tried, 2 for the first fallback, ...
    attempt        integer NOT NULL CHECK (attempt >= 1),
    -- ok: answered in full. failed: failed before any output (the gateway may fall back).
    -- interrupted: failed after output had started. cancelled: the caller went away.
    status         text NOT NULL CHECK (status IN ('ok', 'failed', 'interrupted', 'cancelled')),
    error_code     text,
    input_tokens   integer NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens  integer NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    -- True when the provider never reported usage (stream cut short) and the gateway estimated it.
    tokens_estimated boolean NOT NULL DEFAULT false,
    -- Millionths of a US dollar, so sums stay exact integers.
    cost_microusd  bigint NOT NULL DEFAULT 0 CHECK (cost_microusd >= 0),
    latency_ms     integer NOT NULL CHECK (latency_ms >= 0),
    -- Time until the first token arrived; null when none did.
    first_token_ms integer CHECK (first_token_ms >= 0),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX llm_usage_workspace_time_idx ON llm_usage (workspace_id, created_at DESC);

-- +goose Down

DROP TABLE llm_usage;
DROP TABLE workspace_ai_settings;
