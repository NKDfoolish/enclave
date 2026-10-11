# Part 0: Foundation

## What was built

- Monorepo with one Go module at the root and a uv-managed Python project in `rag/`.
- `compose.yaml` for Postgres, Redis (AOF on) and Qdrant, each with a healthcheck so `make up` waits until they are ready.
- `proto/enclave/rag/v1/rag.proto` with a `Ping` RPC; `buf generate` writes Go to `gen/go` and Python to `rag/gen`.
- Python gRPC server (`rag/src/rag`) and Go client (`api/internal/ragclient`) with a per-call timeout.
- goose migrations in `api/migrations`, a Makefile, and GitHub Actions for proto, Go, Python and an end-to-end smoke test.

## Things that bit

- **Port clash:** 6379 was already used by an SSH tunnel, so local Redis runs on 6380.
- **Python generated imports:** grpc's Python plugin emits absolute imports (`from enclave.rag.v1 import rag_pb2`), so `rag/gen` must be on `PYTHONPATH`. pytest gets it from `pythonpath` in `pyproject.toml`; running the server needs `PYTHONPATH=src:gen`.
- **Killing background processes:** `(cd rag && uv run ...) &` gives the PID of the subshell, so `kill` left `uv` and `python` running and the script hung. Start `uv run --directory rag ...` directly; uv forwards SIGTERM to the child.
- **Runtime vs. codegen versions:** generated Python checks the protobuf runtime version at import time. Keep `grpcio`/`protobuf` in `rag/pyproject.toml` at or above the plugin versions pinned in `buf.gen.yaml`.

## Concepts worth revisiting

- `grpc.NewClient` connects lazily; the first RPC triggers the dial. Errors such as "connection refused" surface as `codes.Unavailable` on the call.
- Deadlines: the Go client sets a timeout per call; the Python server sees it as the RPC deadline. Test it with `bufconn`, an in-memory listener.
- Graceful shutdown on both sides: Go `http.Server.Shutdown`, Python `server.stop(grace=...)`.

## Next

Part 1: workspaces, login and roles. Decide early how the Core API passes verified identity to downstream services.
