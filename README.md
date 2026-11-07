# Enclave

A private document workspace: businesses keep a document library, share selected documents with their clients, and ask questions answered with source citations under enforced permissions.

The full plan is in [ENCLAVE_PLAN_EN.md](ENCLAVE_PLAN_EN.md). Notes from each part go in [docs/learnings/](docs/learnings/); architecture decisions in [docs/adr/](docs/adr/); coding rules in [docs/conventions.md](docs/conventions.md).

## Repository layout

| Path | What it is | Status |
|---|---|---|
| `api/` | Core API (Go): accounts, JWT auth, workspaces, roles. Layout: [ADR 0001](docs/adr/0001-go-code-structure.md) | Part 1 |
| `rag/` | RAG service (Python, gRPC) | Part 0 skeleton |
| `proto/` | Protobuf contracts between services | `rag.v1` |
| `gen/go/`, `rag/gen/` | Generated code (`make proto`, committed) | |
| `web/` | Browser UI (plain HTML/JS, embedded in the API binary) | Part 1 |
| `gateway/`, `mcp/`, `agent/`, `inference/`, `infra/` | Later parts | Not started |

Go services share one module at the repo root (`go.mod`).

## Prerequisites

Go 1.25+, [uv](https://docs.astral.sh/uv/), Docker with Compose, [buf](https://buf.build/docs/installation).

## Getting started

```bash
cp .env.example .env   # optional; defaults work unless ports clash
make up                # Postgres :5432, Redis :6380, Qdrant :6333/6334 (the API needs Postgres and Redis)
make setup             # Python dependencies
make migrate-up
make test              # unit tests + Go integration tests against enclave_test
make smoke             # Go API -> Python RAG over gRPC
```

Run `make help` for all commands. Start the API with `make run-api` and open http://localhost:8080
to register, create a workspace and invite members. `make run-rag` starts the RAG service.

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

Non-members get `404` for anything inside a workspace, so workspace IDs cannot be probed.
Authentication design: [ADR 0002](docs/adr/0002-jwt-authentication.md).
