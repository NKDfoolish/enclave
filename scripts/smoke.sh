#!/usr/bin/env bash
# End-to-end check for Part 0: the Go Core API calls the Python RAG service over gRPC.
set -euo pipefail

cd "$(dirname "$0")/.."
RAG_PORT="${RAG_PORT:-50051}"
API_PORT="${API_PORT:-8080}"
mkdir -p bin

cleanup() {
  [[ -n "${API_PID:-}" ]] && kill "$API_PID" 2>/dev/null || true
  [[ -n "${RAG_PID:-}" ]] && kill "$RAG_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT

PYTHONPATH=src:gen RAG_GRPC_LISTEN="127.0.0.1:${RAG_PORT}" uv run --quiet --directory rag python -m rag.main &
RAG_PID=$!

go build -o bin/api ./api/cmd/api
API_ENABLE_DEBUG_ROUTES=true API_HTTP_ADDR="127.0.0.1:${API_PORT}" RAG_GRPC_ADDR="127.0.0.1:${RAG_PORT}" ./bin/api &
API_PID=$!

for _ in $(seq 1 30); do
  if body=$(curl -fsS "http://127.0.0.1:${API_PORT}/v1/debug/rag-ping" 2>/dev/null); then
    echo "OK: $body"
    exit 0
  fi
  sleep 0.5
done

echo "FAIL: api could not reach rag within 15s" >&2
exit 1
