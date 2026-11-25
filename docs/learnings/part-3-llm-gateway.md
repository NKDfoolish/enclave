# Part 3: LLM Gateway — Summary

Branch: `part-3-llm-gateway`. Flow diagrams: [docs/flows.md](../flows.md) sections 21–28.
Decisions and trade-offs: [ADR 0004](../adr/0004-llm-gateway.md).

The plan's "Done when" criteria:

- [x] At least two providers stream through the gateway; embeddings work through a supported
  provider. Live: Ollama (`llama3.2:3b`) and NVIDIA NIM (`nemotron-3.5-bolt-e4b`,
  `mistral-large-4`) stream; embeddings through Ollama (`nomic-embed-text`, 768 dimensions) and
  NVIDIA (`nemotron-3.5-embed-1b`, 2048 dimensions). Anthropic's adapter is tested against a
  fake server replaying its event format; it turns on when `ANTHROPIC_API_KEY` is set.
- [x] Timeout, cancellation and approved fallback are tested, and gateway latency is recorded
  with k6 (below).

Tests: `gateway/internal/llm/app` (11 unit tests with hand-written fakes), adapters against
real captured provider streams, `gateway/internal/llm/infra` (Postgres, Redis), and
`gateway/cmd/gateway` (8 end-to-end tests: real routes, Postgres and Redis, fake providers).
Core API: `api/cmd/api/ai_settings_test.go`.

---

## 1. What was built

```text
gateway/                          NEW service
├── catalog.json                  providers and aliases (no secrets)
├── bench/catalog.json            one alias pointing at the fake provider
├── cmd/gateway/                  composition root, routes, end-to-end tests
├── cmd/fakeprovider/             instant OpenAI-format provider for benchmarks (dev only)
└── internal/
    ├── config/                   env settings, service tokens, catalog loading + validation
    └── llm/
        ├── domain/               ChatRequest, Chunk, Usage, Catalog/Alias/Route/Price,
        │                         Policy, ProviderError (retryable or not), UsageRecord
        ├── app/                  routing, fallback, first-token timeout, accounting
        ├── infra/                Postgres (policy, usage), Redis token bucket (Lua)
        │   ├── openai/           net/http adapter + SSE reader: Ollama, NVIDIA, OpenAI
        │   └── anthropic/        official SDK adapter
        └── transport/            OpenAI-format HTTP + SSE writer, service-token auth
api/internal/workspace/*/ai.go    AI settings: who may allow which external providers
internal/platform/                moved from api/internal/platform (shared by both services)
internal/platform/env/            NEW: env reader that collects every config error
migrations/00004                  workspace_ai_settings, llm_usage
web/static                        "AI providers" panel (admin edits, staff reads)
scripts/k6/gateway.js             benchmark; scripts/bench-gateway.sh runs it
```

| Aliases (`gateway/catalog.json`) | Routes, in fallback order |
|---|---|
| `chat-fast` | Ollama `llama3.2:3b` → NVIDIA `nemotron-3.5-bolt-e4b` |
| `chat-smart` | NVIDIA `mistral-large-4` → Anthropic `claude-opus-5-5` → Ollama `llama3.2:3b` |
| `embed-local` | Ollama `nomic-embed-text` (no fallback, ever) |
| `embed-nvidia` | NVIDIA `nemotron-3.5-embed-1b` (no fallback, ever) |

## 2. Design decisions

| Decision | Why |
|---|---|
| Separate service, OpenAI-format API | Python callers use the standard `openai` client; secrets, limits and accounting live in one place. |
| Aliases instead of provider model names | NVIDIA's free Kimi model did not answer within 90 s in a live test; switching `chat-smart` to Mistral was one line in the catalog. A model showed "Deprecation in 1d" on NVIDIA's site. |
| Hand-written adapter for the OpenAI format, SDK for Anthropic | The OpenAI format is shared by many providers and simple to read; Anthropic has an official Go SDK, and the SDK knew things a guess got wrong (it sends the key as a bearer token). |
| Local providers always allowed, external ones only with admin approval | A workspace's content leaves Enclave only if its admin agreed. The policy filters routes, so fallback cannot sneak around it. |
| Fallback only before the first event | After that the caller has part of an answer; a second model would start over and the result would be garbled. |
| First-token timeout cancels the attempt with a cause | `context.WithCancelCause` lets the gateway tell "provider silent" (retryable) apart from "caller left" (stop). |
| Any provider event counts as the first token | A reasoning model can think for a long time before visible text; that is liveness, not a hang. |
| Embedding aliases have exactly one route | 768-dimension and 2048-dimension vectors (or two 768s from different models) are not comparable. |
| Token bucket in a Lua script, Redis clock | Atomic under concurrency (50 parallel requests, burst 10: exactly 10 allowed) and consistent across instances. |
| Fail closed when Redis is down | No limiter means one workspace could run up any cost. |
| One usage row per attempt, estimated tokens flagged | Failed and retried calls are visible; a stream cut short is still charged. An attempt that produced nothing is charged nothing. |
| Service tokens per caller, constant-time comparison | Usage is attributed to the calling service; tokens rotate per service; timing reveals nothing. |
| Shared plumbing moved to the root `internal/` | Go's `internal/` rule: `api/internal` is importable only from `api/`. Moved when the second service needed it. |

## 3. Concepts worth knowing

- **Server-Sent Events**: a long HTTP response of `data: ...` lines separated by blank lines.
  The server must flush after each event (`http.ResponseController.Flush`), and proxies must not
  buffer (`X-Accel-Buffering: no`). Once the first byte is out, the status code is fixed at 200:
  later errors can only be reported inside the stream.
- **Context trees**: request context → whole-request timeout → per-attempt context with cause.
  Cancelling a parent cancels every child, which is how a closed browser tab stops a model on
  another machine.
- **`context.WithoutCancel`**: keep writing the usage row after the caller left, because the
  provider may already have billed.
- **Retryable vs non-retryable errors**: retry what another provider could do better (network,
  timeout, 429, 5xx, a provider's own 401/404); never retry what the request caused (400, 422).
- **Token bucket**: capacity = burst, refill rate = sustained rate; `retry_after` comes out of
  the same arithmetic.
- **Reasoning tokens**: billed as output, invisible in the answer. With `max_tokens: 250`,
  Mistral Large 4 spent all 250 on thinking and returned no text (`finish_reason: length`). A
  three-sentence answer cost 972 output tokens and took about 2 minutes.
- **Embedding encodings**: OpenAI's Python client asks for `base64` (little-endian float32) by
  default; a gateway that only returns float lists breaks it.

## 4. Measurements

`make bench-gateway`: 10 virtual users, 20 s direct to a fake provider, then 20 s through the
gateway to the same provider (236,437 requests, 100% successful). Milliseconds:

| | p50 | p95 | p99 |
|---|---|---|---|
| Full streamed response, direct | 1.06 | 1.76 | 2.41 |
| Full streamed response, via gateway | 3.28 | 4.53 | 5.58 |
| **Gateway overhead** | **2.2** | **2.8** | |

Live calls (`llm_usage` rows): Ollama first token 61 ms (model loaded) to 2 s (cold load);
NVIDIA `nemotron-3.5-bolt-e4b` first token 0.8 s; Mistral Large 4 first byte 1–1.6 s.

## 5. Things found while building

| Finding | Resolution |
|---|---|
| Providers that "speak OpenAI" still differ: Ollama sends usage in a chunk with empty `choices`, NVIDIA adds `logprobs` and reasoning fields, NVIDIA embeddings need `input_type`, 768 vs 2048 dimensions | Adapter tests replay real captured streams from both. |
| A first-token timeout wraps `context.Canceled` (the timer cancels the attempt), so `errors.Is(err, context.Canceled)` wrongly looked like "caller left" and the handler wrote nothing | Ask the request's own context (`r.Context().Err()`) whether the caller left. |
| Live test: failed attempts (connection refused) were charged estimated input tokens | Only estimate when the provider produced output; otherwise zero. Unit test added. |
| NVIDIA's free Kimi endpoint did not answer for 90 s; the gateway cut it at 30 s and fell back as designed | `chat-smart` now starts with Mistral Large 4. |
| The Anthropic SDK reads `ANTHROPIC_BASE_URL` and other env vars by default (this machine has one set) | `option.WithoutEnvironmentDefaults()`; the catalog is the only configuration. |
| `go test` hung in a mutation run (fake provider waiting for a cancel that never came) | Mutation runs use `-timeout 60s`. |
| Mutation checks | Removing each of these failed its test (13/13): no fallback after output, policy check, first-token timer, timeout retryability, cancelled status, rate limit, non-retryable 400, cancel propagation, Lua decrement, unknown workspace, admin-only AI settings, truncated-stream detection, zero billing for failed attempts. |

## 6. Deliberately left for later

- Tool calling and multimodal messages (the agent, Part 7, needs tools).
- Per-workspace token or cost budgets on top of the request rate limit.
- Circuit breakers (skip a provider that keeps failing), caching, hedged requests.
- Metrics and traces (OpenTelemetry, Part 9); a usage report endpoint for admins.
- RAG (Part 4) is the first real caller; it will choose between `embed-local` and
  `embed-nvidia` once and store the model name with every vector.
