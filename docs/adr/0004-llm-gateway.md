# ADR 0004: LLM Gateway

- Status: accepted
- Date: 2026-11-22

## Context

From Part 4 on, the RAG service and later the agent call language models for embeddings and
answers. Without a single place for those calls:

- provider API keys would spread over several services;
- every service would handle each provider's format, errors and limits itself;
- nobody could say what a workspace's AI use costs, or stop one workspace from using it all;
- a workspace's questions and document excerpts could reach a provider outside Enclave without
  the workspace admin ever agreeing to it.

Flows: [docs/flows.md](../flows.md), sections 21–28.

## Decision

**A separate Go service with an OpenAI-format API.** `POST /v1/chat/completions` (streamed as
Server-Sent Events or answered at once), `POST /v1/embeddings` and `GET /v1/models`. Python
services use the standard `openai` client with the gateway's URL. Callers authenticate with a
per-service bearer token (compared in constant time) and name the workspace in
`X-Enclave-Workspace-ID`; they have already checked the user's access. The gateway is internal
only.

**Aliases, not provider model names.** Callers ask for `chat-fast`, `chat-smart`, `embed-local`.
`gateway/catalog.json` maps each alias to ordered routes (provider + provider model + price per
million tokens). Changing or retiring a model is a catalog change, not a change in every
caller. Providers without an API key set are disabled at startup and their routes skipped.

**Two adapters.** Ollama, NVIDIA NIM and OpenAI speak OpenAI's wire format; one adapter written
with `net/http` covers them, with options for their differences (NVIDIA's `input_type` for
embeddings). Anthropic's Messages API differs (separate system field, required `max_tokens`,
typed stream events, no sampling parameters on current models) and uses the official Go SDK,
with environment defaults and SDK retries turned off so only the catalog configures it and only
the gateway decides about retries.

**Workspace AI settings decide where content may go.** Providers are local (inside Enclave) or
external. A new workspace allows no external provider. An admin lists the allowed ones through
the Core API (`/v1/workspaces/{ws}/ai-settings`, table `workspace_ai_settings`). The gateway
reads the setting on every call and drops routes to providers that are not allowed, so content
never reaches an unapproved provider, fallbacks included.

**Fallback only before output begins.** Routes are tried in order. A failure moves to the next
route only if nothing has reached the caller yet and another provider could plausibly succeed:
no connection, a timeout, 429, 5xx, and 401/403/404 (a broken key or a retired model at that
provider). 400, 413 and 422 blame the request and are returned at once. Once the first event has
been forwarded the gateway is committed: a later failure is reported inside the stream as an
error event, without `[DONE]`.

**First-token timeout.** Each attempt has a timer (30 s by default) that cancels it if the
provider sends no event at all, so a stuck provider leaves time to fall back. Any event counts,
including a reasoning model's thinking, because it proves the provider is working. A whole
request, fallbacks included, is bounded by a request timeout (5 min).

**Cancellation.** Provider calls run under the request's context; when the caller disconnects,
the provider request is aborted and the model stops generating.

**Embeddings never fall back.** Vectors from different models live in different spaces; mixing
them would silently corrupt search. The catalog rejects an embedding alias with more than one
route.

**Rate limit: token bucket per workspace in Redis**, one Lua script (refill, take, save,
expire) so concurrent requests through any number of gateway instances stay consistent; the
clock is Redis's own. 429 with `Retry-After` when empty. If Redis is unreachable the gateway
refuses requests (fail closed) rather than run without a limit.

**Accounting: one `llm_usage` row per provider attempt**, written even when the caller left:
workspace, caller, request ID, alias, provider and model, attempt number, status (`ok`, `failed`,
`interrupted`, `cancelled`), tokens, cost in integer micro-dollars, latency, time to first
token. Provider-reported tokens are used when available; when a stream is cut short after
output began, tokens are estimated from the text (about four characters per token) and flagged
`tokens_estimated`. An attempt that produced nothing records zero tokens.

**No content in logs.** Logs and usage rows never hold prompts or answers. Usage rows keep a
stable error code; a provider's own error message (cut to 200 characters) appears only in the
gateway's logs, and callers get a stable code such as `provider_error`.

## Consequences

- One place to change providers, keys, prices and limits. The measured cost of that hop is
  about 2 ms at p50 and 3 ms at p95 (k6, 236k streamed requests against a fake provider:
  `make bench-gateway`), small next to model latency (0.3 s to minutes).
- **Accepted coupling:** the gateway reads two Core API tables (`workspaces`,
  `workspace_ai_settings`) directly instead of calling the Core API. It is a read of one row by
  primary key on every call, which keeps policy changes immediate without a cache or a new
  internal API. In production the gateway's database role gets `SELECT` on those tables only.
  Revisit if the gateway gets its own database.
- Migrations moved from `api/migrations` to `migrations/`: two services share one database and
  one migration history.
- Costs from free endpoints (NVIDIA NIM, local Ollama) are recorded as 0; their real price is
  capacity and rate limits. Reasoning models spend output tokens on thinking that callers never
  see (972 output tokens for a three-sentence answer in a live test), so `max_tokens` must leave
  room for it.
- The gateway does not use Anthropic's server-side refusal fallback: a refusal is reported to the
  caller as `finish_reason: content_filter`, and switching models stays the gateway's decision so
  accounting always names the model that answered.
- Not done yet: tool calling and multimodal messages (needed by the agent, Part 7), per-workspace
  token or cost budgets, circuit breakers, caching, OpenTelemetry metrics (Part 9). Shutdown waits
  up to the shutdown timeout for open streams, then cuts them.
