# Part 0: Foundation — Summary

Completed: 2026-10-11 · CI green on `main`.

The plan's "Done when" criteria:

- [x] One command starts local infrastructure, and a Go service calls a Python service over gRPC (`make up`, `make smoke`).
- [x] CI runs linting and tests for Go and Python ([ci.yml](../../.github/workflows/ci.yml)).

---

## 1. Architecture

### 1.1 Whole system (v1.0 target)

```text
                    ┌──────────── ENCLAVE ─────────────────────────────────────────┐
 Users ─HTTPS─▶     │  Web UI ──HTTP──▶ Core API (Go) ──gRPC──▶ RAG (Python)       │
 (admin/staff/      │                     │    │                  │                 │
  client)           │                     │    └─gRPC─▶ Agent (Python/LangGraph)    │
 MCP client ─MCP─▶  │  MCP server (Go) ───┘                       │                 │
                    │                                              ▼                 │
                    │                         LLM Gateway (Go) ──▶ OpenAI/Anthropic  │
                    │                                │         ──▶ Ollama / vLLM     │
                    │  Postgres   Redis   Qdrant   S3                                │
                    └───────────────────────────────────────────────────────────────┘
```

Style: **polyglot microservices** talking internally over **gRPC/Protobuf**.

### 1.2 Built in Part 0

```text
curl ──HTTP──▶ Core API (Go, :8080) ──gRPC Ping──▶ RAG (Python, :50051)

Docker Compose:  Postgres :5432   Redis :6380   Qdrant :6333/6334   (running, not used yet)
CI:              GitHub Actions → proto / go / python / smoke
```

### 1.3 Inside the Core API

A **modular monolith**, split by **bounded context (DDD)**, with **Clean/Hexagonal Architecture** inside each module.
Decision and rationale: [ADR 0001](../adr/0001-go-code-structure.md).

```text
api/
├── cmd/api/              composition root: main.go (wiring) + routes.go (endpoint index)
├── internal/
│   ├── <context>/        identity, workspace, document, qa…   (from Part 1)
│   │   ├── domain/         entities, business rules, Repository interfaces
│   │   ├── app/            use cases
│   │   ├── infra/          Postgres, S3, Redis
│   │   └── transport/      HTTP handlers
│   └── platform/         shared plumbing, no business rules
│       ├── config/         env → Config, reports every error at startup
│       ├── httpserver/     server + graceful shutdown, middleware, WriteJSON/WriteError
│       ├── diagnostics/    /healthz, /v1/debug/rag-ping
│       └── ragclient/      gRPC client for RAG
└── migrations/           goose

Dependencies:  transport ──▶ app ──▶ domain ◀── infra
```

Compared with traditional layering (`handler/service/repository`):

1. Folders are split **by feature first, layer second**.
2. **The domain declares the interfaces** and infra implements them (Dependency Inversion), so business logic does not depend on the database and can be tested without one.

Where new code goes:

| Writing | Goes in |
|---|---|
| Business rules, entities | `<context>/domain/` |
| A user action (use case) | `<context>/app/` |
| SQL, S3, Redis | `<context>/infra/` |
| HTTP handlers | `<context>/transport/` |
| Shared plumbing | `platform/` |
| Registering routes / wiring dependencies | `cmd/api/routes.go` / `cmd/api/main.go` |
| Schema changes | `migrations/` (`make migrate-new name=...`) |

### 1.4 Core API runtime flow

```text
STARTUP
main() → run()
  ① config.Load()            invalid → return err → main: exit 1
  ② ragclient.New()          gRPC client (lazy)            + defer rag.Close()
  ③ signal.NotifyContext()   Ctrl+C/SIGTERM → cancel ctx   + defer stop()
                             + goroutine: first signal → stop() (second one kills)
  ④ routes()                 mux + RequestID → AccessLog → Recover
  ⑤ httpserver.Run()         go ListenAndServe; select { error | ctx.Done() }

EACH REQUEST (its own goroutine)
  RequestID → AccessLog → Recover → mux → handler → (ragclient → gRPC → Python)
  ← response; AccessLog writes one log line

SHUTDOWN
  ctx.Done() → srv.Shutdown (wait ≤ ShutdownTimeout) → return nil
  → defer stop() → defer rag.Close() → main: exit 0
```

| Ends because | Path | Exit code |
|---|---|---|
| Invalid config | ① return err, no defers registered yet | 1 |
| Port already in use | ⑤ errCh → stop() → rag.Close() | 1 |
| Ctrl+C / SIGTERM | ⑤ ctx.Done → Shutdown → stop() → rag.Close() | 0 |

---

## 2. Knowledge by topic

### 2.1 Project layout and tooling

| Tool | Role | Remember |
|---|---|---|
| Monorepo | All services in one repo | One Go module at the root, shared by api/gateway/mcp |
| [Makefile](../../Makefile) | Shared shortcuts for people and CI | `.PHONY`; recipes indented with Tab; each line runs in its own shell; `-include .env` + `export`; `?=` |
| uv | Python manager | `pyproject.toml` ≈ `go.mod`, `uv.lock` ≈ `go.sum`; `--locked` in CI |
| buf | Proto lint, codegen, breaking-change checks | `buf.yaml` = sources and rules; `buf.gen.yaml` = outputs; pin plugin versions |
| goose | Migrations | Numbered `Up`/`Down` files; `goose_db_version` table; each file runs in a transaction; never edit a pushed migration, add a new one |
| golangci-lint, ruff | Linters | Caught real bugs: ignored `Close()` error, comparing errors with `==` |

Why a migration tool beats editing the database by hand: you know which version each database is at, every environment converges to the same schema, upgrades keep existing data, rollbacks are written and tested in advance, history lives in git, and it all automates.
What it does not solve by itself: table locks from `ALTER` on large tables, backward compatibility during deploys (expand/contract), and data loss from `DROP COLUMN`.

### 2.2 Docker Compose ([compose.yaml](../../compose.yaml))

- Image (package) / container (running instance) / volume (data outside the container) / port mapping `host:container`.
- Healthcheck + `up --wait`: wait until services are **actually ready**, not just "running".
- Named volumes survive container removal; `down -v` deletes them.
- `POSTGRES_*` variables only apply on **first initialization**.
- Redis runs with **AOF** so queued jobs survive a restart. The Qdrant image has no `curl`, so its healthcheck uses `/dev/tcp`.
- `$$` stops Compose from substituting a variable. Containers reach each other by **service name**.

### 2.3 Protobuf and gRPC

- Protobuf is a language-neutral data format; gRPC is RPC over HTTP/2. They are different things.
- One `.proto` generates code for both Go and Python, so the two sides cannot drift apart.
- Data is sent by **field number**. Changing or removing a field number is a breaking change (no error, data silently disappears). Adding a new field with a new number is safe.
- RPC path: `/enclave.rag.v1.RagService/Ping`. A call needs an **address** (env var) and a **contract** (generated code).
- `grpc.NewClient` connects lazily. The client's deadline travels over the wire to the server.
- Enum value 0 must be `_UNSPECIFIED` (otherwise "forgot to send a role" means ADMIN).
- Generated code is committed; CI checks it is not stale (`git status --porcelain`, which also catches new files).

### 2.4 Go core concepts

| Concept | Lesson |
|---|---|
| `main` → `run() error` | `os.Exit` skips `defer`, so only `main` calls `os.Exit`; `run` returns errors upward |
| Errors are values | Return early; wrap with `%w`; compare with `errors.Is`; combine with `errors.Join` |
| `defer` | Pushes a deferred call onto a stack that runs when the function returns (including on panic), in LIFO order. Arguments are evaluated at the `defer` line; closures read values when they run. Scoped to the function, not the block (beware in loops). Does not run on `os.Exit`. Open-coded since Go 1.14, so nearly free |
| Context | `cancel()` **closes** the `Done()` channel, waking every waiter at once. Cancellation propagates to child contexts. Cancel signals; it does not force |
| `(ctx, cancel)` | Pass `ctx` down; `defer cancel()` right where it was created, or timers and goroutines leak |
| Goroutines, channels, `select` | The server runs in its own goroutine; `select` waits for "error or shutdown signal"; a buffered channel keeps the sender from blocking |
| OS signals | `NotifyContext` intercepts SIGINT/SIGTERM via a channel plus a watcher goroutine. `stop()` unregisters and restores default handling. Call `stop()` right after the first signal so a second one kills the process |
| Graceful shutdown | `srv.Shutdown`: stop accepting, wait for in-flight requests, with its own timeout (uses `context.Background()` because the old ctx is already cancelled) |
| Implicit interfaces | Declare interfaces **on the consumer side** (`RAGPinger`): "accept interfaces, return structs" |
| Closures, handler factories | `RAGPing(logger, rag)` returns a handler; no globals |
| Embedding | `statusRecorder` embeds `ResponseWriter`; the fake gRPC server embeds `Unimplemented...Server` |
| Encapsulation | Uppercase = exported, lowercase = unexported; the compiler blocks imports of `internal/` from outside |
| Resources | Anything that starts goroutines or holds sockets has a `Close()`, and `Close()` waits for its goroutines to exit. Whoever creates it cleans it up |

Under `rag.Close()`: move to Shutdown state → close resolver/balancer → send HTTP/2 GOAWAY → `close()` the socket → wait for reader/writer goroutines to exit.
Under `stop()`: `cancel()` (the watcher goroutine exits if still waiting) + `signal.Stop` (remove from the registry; when nobody listens any more, restore default handling).

### 2.5 HTTP with the standard library (no framework)

- Go 1.22+ `ServeMux` supports `"GET /path"` and `{id}`.
- **Middleware = `func(http.Handler) http.Handler`** (Decorator). `Chain` wraps in reverse so the first one listed is outermost.
  - **RequestID:** correlation ID stored in the context under a private key; client input is validated to prevent log injection.
  - **AccessLog:** an "eavesdropping" writer captures the status; logs the path only, **never the query string or body**; log level follows the status.
  - **Recover:** `defer` + `recover()`; logs the stack trace; returns a 500 without leaking details; must sit **inside** AccessLog.
- Responses: Header → `WriteHeader` → Body. Standard error body `{code, message, request_id}`. 502 when a downstream service fails.
- Security: `ReadHeaderTimeout` (Slowloris), `IdleTimeout`, debug routes off by default, detailed logs but generic client messages.
- Config: fail fast, report all errors at once, secure defaults. Logging: `slog` JSON, key–value attributes.

Framework usage (JetBrains 2025): Gin 48%, net/http ~32%, Echo 16%, chi ~12%, Fiber 11%. This project uses net/http to learn the internals; chi is the natural next step if needed (100% net/http compatible).

### 2.6 Python

- src layout + `PYTHONPATH=src:gen`; generated code uses absolute imports.
- `grpcio`: subclass the `Servicer`; `ThreadPoolExecutor`; `add_insecure_port("...:0")` to get a free port in tests.
- Graceful shutdown: `signal.signal` + `server.stop(grace=5)`.
- pytest fixtures with `yield`: setup → test → teardown.
- No web framework because RAG only speaks gRPC. For Part 4, consider `grpc.aio`, `pydantic-settings`, and JSON logs carrying the request ID.

### 2.7 Testing

| Kind | Example | Checks |
|---|---|---|
| Unit with in-memory fakes | `bufconn`, `fakePinger`, `httptest.NewRecorder` | One component; fast; no network |
| Table-driven | `TestRequestID`, `TestRAGPing` | Many cases; adding one is a single line |
| Failure paths | timeout, panic, RAG down, invalid config | As important as the happy path |
| Smoke / end-to-end | [scripts/smoke.sh](../../scripts/smoke.sh) | The seams between services; caught the `PYTHONPATH` bug that unit tests missed |

Smoke script: `set -euo pipefail`; `trap cleanup EXIT` + `wait`; bounded polling; `curl -f` so 5xx counts as failure.

### 2.8 CI/CD with GitHub Actions

- GitHub picks up `.github/workflows/*.yml` automatically. Jobs run in parallel on fresh VMs; `needs` orders them.
- An action is a repo with an `action.yml`; `@v7` is a tag. Read release notes before a major bump (`setup-uv` dropped major tags in v8, so it is pinned as `v10.2.0`).
- `permissions: contents: read` (least privilege); `concurrency` (cancel superseded runs); `uv sync --locked`; `go test -race`.
- Read the annotations after each run: the Node 20 warnings led to the version bumps.

### 2.9 Git and operational security

- Several accounts on one machine: per-repo identity (`git config --local`), SSH aliases with `IdentitiesOnly yes`, remotes use the alias (`github-game`) rather than `github.com`.
- `ssh-agent` holds decrypted keys; `--apple-use-keychain` stores the passphrase in the macOS Keychain.
- Never commit `.env`, `bin/`, `.venv`. Scan for secrets before committing.
- ADRs record architecture decisions and their reasons.

---

## 3. Bugs actually hit

| Bug | Cause | Lesson |
|---|---|---|
| Redis would not start | Port 6379 was held by an SSH tunnel | Check with `lsof`; make ports configurable via `.env` |
| RAG did not start in the smoke test | Missing `PYTHONPATH` (pytest adds it, so unit tests passed) | Test environment ≠ runtime environment, hence the smoke test |
| Smoke script hung | `(cd … && uv run …) &` made `$!` the subshell's PID | Make the PID point at the real process; `trap` + `wait` |
| Python failed at import | Protobuf runtime older than the codegen plugin | Keep `grpcio`/`protobuf` ≥ the plugin versions in `buf.gen.yaml` |
| Lint flagged `defer rag.Close()` | Ignored returned error | Every error is handled or logged |
| `errorlint` flagged `v == ErrAbortHandler` | Comparing errors with `==` | Use `errors.Is` |
| Second Ctrl+C did nothing | `stop()` only ran in `defer` | Read library source to understand real behaviour |
| SIGINT-based signal test gave wrong results | Bash ignores SIGINT for background jobs | Verify the test method, not just the code |
| Node 20 warnings in CI | Outdated actions | Read annotations; upgrade with verification |

---

## 4. Self-check interview questions

1. How does middleware work in Go? Why does the order of Recover and AccessLog matter?
2. What does your graceful shutdown do? How is a second signal handled?
3. How does context cancellation work underneath? Why always `defer cancel()`?
4. When does `defer` run, in what order, and when does it not run?
5. How does gRPC differ from REST? How do you change a proto without breaking old clients?
6. Why use a migration tool? How do you roll back? How do you add a column to a large table without downtime?
7. What architecture is the code organized in? How does it differ from handler/service/repository?
8. How do you trace the logs of one request across services?
9. Why not log query strings? Why return generic errors to clients?
10. How do unit and smoke tests differ? Give an example of a bug unit tests missed.

---

## 5. Next: Part 1

Workspaces and access control: login/logout (sessions), workspaces, member invitations, admin/staff/client roles, tenant isolation (including guessed IDs).
First bounded contexts `identity/` and `workspace/`, Postgres via pgx, hand-written SQL, and a hand-written `Authenticate` middleware.
Decide early: how the Core API passes verified identity to downstream services.
