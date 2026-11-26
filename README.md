# Enclave

A private document workspace: businesses keep a document library, share selected documents with their clients, and ask questions answered with source citations under enforced permissions.

The full plan is in [ENCLAVE_PLAN_EN.md](ENCLAVE_PLAN_EN.md). Notes from each part go in [docs/learnings/](docs/learnings/); architecture decisions in [docs/adr/](docs/adr/); coding rules in [docs/conventions.md](docs/conventions.md); every runtime flow as a sequence diagram in [docs/flows.md](docs/flows.md).

## Repository layout

| Path | What it is | Status |
|---|---|---|
| `api/` | Core API (Go): accounts, JWT auth, workspaces, roles, document library, AI settings, conversations. Layout: [ADR 0001](docs/adr/0001-go-code-structure.md) | Part 4 |
| `gateway/` | LLM Gateway (Go): OpenAI-format chat and embeddings over Ollama, NVIDIA NIM and Anthropic, with fallback, rate limits and usage accounting. [ADR 0004](docs/adr/0004-llm-gateway.md) | Part 3 |
| `internal/platform/` | Go plumbing shared by the services (HTTP server, Postgres, Redis, config, ...) | |
| `migrations/` | SQL migrations for the shared Postgres database (goose) | |
| `rag/` | RAG service (Python): gRPC server that answers with citations, ingestion worker that indexes documents into Qdrant, evaluation in `rag/eval/`. [ADR 0005](docs/adr/0005-rag-and-qa.md) | Part 4 |
| `proto/` | Protobuf contracts between services | `rag.v1` |
| `gen/go/`, `rag/gen/` | Generated code (`make proto`, committed) | |
| `web/` | Browser UI (plain HTML/JS, embedded in the API binary) | Part 4 |
| `mcp/`, `agent/`, `inference/`, `infra/` | Later parts | Not started |

Go services share one module at the repo root (`go.mod`).

## Prerequisites

Go 1.25+, [uv](https://docs.astral.sh/uv/), Docker with Compose, [buf](https://buf.build/docs/installation).

## Getting started

```bash
cp .env.example .env   # first-time setup
# Generate a shared local service credential for the API and RAG processes.
printf 'RAG_SERVICE_TOKEN=%s\n' "$(openssl rand -hex 32)" >> .env
make up                # Postgres :5432, Redis :6380, Qdrant :6333/6334, object storage :9000 (RustFS)
make setup             # Python dependencies
make migrate-up
make test              # unit tests + Go integration tests against enclave_test
make smoke             # Go API -> Python RAG over gRPC; gateway starts with the real catalog
```

Run `make help` for all commands. Start the API with `make run-api` and open http://localhost:8080
to register, create a workspace, invite members, upload documents and choose which external AI
providers the workspace allows.

### Questions and answers

Five processes, each in its own terminal:

```bash
ollama serve           # local models (see below for the one-time setup)
make run-gateway       # LLM Gateway :8090
make run-api           # Core API :8080 and the web UI
make run-rag           # RAG gRPC server :50051 (answers)
make run-rag-worker    # RAG ingestion worker (indexes documents)
```

Upload a document, wait until its status says "searchable", then ask in "Ask the documents".
`make rag-eval` runs the 42-question evaluation against a sample corpus (add `ARGS=--ragas` for
Ragas scores); `make rag-reindex` re-queues every document after changing the embedding model.

### LLM Gateway

```bash
brew install ollama k6                                # once
ollama pull llama3.2:3b && ollama pull nomic-embed-text
ollama serve                                          # in its own terminal, only while needed
make run-gateway                                      # :8090
make bench-gateway                                    # latency the gateway adds (k6, fake provider)
```

External providers need their key in `.env` (`NVIDIA_API_KEY`, `ANTHROPIC_API_KEY`); without
one, the provider is disabled and its routes are skipped. A call from a service:

```bash
curl -N localhost:8090/v1/chat/completions \
  -H "Authorization: Bearer enclave-dev-gateway-token" \
  -H "X-Enclave-Workspace-ID: <workspace id>" \
  -H "Content-Type: application/json" \
  -d '{"model": "chat-fast", "stream": true, "messages": [{"role": "user", "content": "Xin chào"}]}'
```

| Method | Path | Notes |
|---|---|---|
| `POST` | `/v1/chat/completions` | OpenAI format; `stream: true` for Server-Sent Events |
| `POST` | `/v1/embeddings` | `input` string or list, optional `input_type` (`query`/`passage`), `encoding_format` `float` or `base64` |
| `GET` | `/v1/models` | aliases from `gateway/catalog.json` |

The response headers `X-Enclave-Provider` and `X-Enclave-Provider-Model` say who answered.

## Core API endpoints

All `/v1` endpoints speak JSON. Requests with a body must send `Content-Type: application/json`.
Errors look like `{"error": {"code": "...", "message": "...", "request_id": "..."}}`.

| Method | Path | Who |
|---|---|---|
| `POST` | `/v1/auth/register` | anyone |
| `POST` | `/v1/auth/login` | anyone; sets the `enclave_access` and `enclave_refresh` cookies |
| `POST` | `/v1/auth/refresh` | holder of a refresh cookie; rotates both tokens |
| `POST` | `/v1/auth/logout` | anyone; revokes the login |
| `GET` | `/v1/me` | logged in |
| `POST`, `GET` | `/v1/workspaces` | logged in (create; list own) |
| `GET` | `/v1/workspaces/{id}` | member |
| `GET` | `/v1/workspaces/{id}/members` | admin, staff |
| `PATCH`, `DELETE` | `/v1/workspaces/{id}/members/{userID}` | admin |
| `POST`, `GET` | `/v1/workspaces/{id}/invitations` | admin |
| `DELETE` | `/v1/workspaces/{id}/invitations/{invitationID}` | admin |
| `POST` | `/v1/invitations/accept` | logged in as the invited email |
| `POST` | `/v1/workspaces/{id}/documents` | admin, staff: register an upload, get a presigned PUT URL |
| `POST` | `/v1/workspaces/{id}/documents/{docID}/complete` | admin, staff: file uploaded, start the content check |
| `GET` | `/v1/workspaces/{id}/documents` | members (clients: shared and ready only); `?cursor=&limit=` |
| `GET` | `/v1/workspaces/{id}/documents/{docID}` | members who can see it |
| `GET` | `/v1/workspaces/{id}/documents/{docID}/download` | members who can see it; 302 to a 60 s presigned URL |
| `PATCH`, `DELETE` | `/v1/workspaces/{id}/documents/{docID}` | admin, staff (`title`, `visibility`) |
| `GET` | `/v1/workspaces/{id}/ai-settings` | admin, staff |
| `PUT` | `/v1/workspaces/{id}/ai-settings` | admin (`external_providers`: which providers outside Enclave may receive content) |
| `POST`, `GET` | `/v1/workspaces/{id}/conversations` | members (start; list own) |
| `GET`, `DELETE` | `/v1/workspaces/{id}/conversations/{conversationID}` | the conversation's owner only |
| `POST` | `/v1/workspaces/{id}/conversations/{conversationID}/messages` | owner: ask; the answer streams as Server-Sent Events (`sources`, `delta`, `done`) |

Non-members get `404` for anything inside a workspace, so workspace IDs cannot be probed.
Authentication design: [ADR 0002](docs/adr/0002-jwt-authentication.md). Document storage: [ADR 0003](docs/adr/0003-document-storage.md). LLM Gateway: [ADR 0004](docs/adr/0004-llm-gateway.md). RAG and Q&A: [ADR 0005](docs/adr/0005-rag-and-qa.md).
