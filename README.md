# Enclave

A private document workspace: businesses keep a document library, share selected documents with their clients, and ask questions answered with source citations under enforced permissions.

The full plan is in [ENCLAVE_PLAN_EN.md](ENCLAVE_PLAN_EN.md). Notes from each part go in [docs/learnings/](docs/learnings/); architecture decisions in [docs/adr/](docs/adr/).

## Repository layout

| Path | What it is | Status |
|---|---|---|
| `api/` | Core API (Go): workspaces, auth, documents. Layout: [ADR 0001](docs/adr/0001-go-code-structure.md) | Part 0 skeleton |
| `rag/` | RAG service (Python, gRPC) | Part 0 skeleton |
| `proto/` | Protobuf contracts between services | `rag.v1` |
| `gen/go/`, `rag/gen/` | Generated code (`make proto`, committed) | |
| `gateway/`, `mcp/`, `agent/`, `inference/`, `web/`, `infra/` | Later parts | Not started |

Go services share one module at the repo root (`go.mod`).

## Prerequisites

Go 1.25+, [uv](https://docs.astral.sh/uv/), Docker with Compose, [buf](https://buf.build/docs/installation).

## Getting started

```bash
cp .env.example .env   # optional; defaults work unless ports clash
make up                # Postgres :5432, Redis :6380, Qdrant :6333/6334
make setup             # Python dependencies
make migrate-up
make test
make smoke             # Go API -> Python RAG over gRPC
```

Run `make help` for all commands. To run the services by hand, use `make run-rag` and `make run-api` in two terminals, then:

```bash
curl localhost:8080/v1/debug/rag-ping
```
