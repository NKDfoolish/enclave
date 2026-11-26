#!/usr/bin/env bash
# End-to-end check: the Go Core API starts against real Postgres, Redis and object storage
# and calls the Python RAG service over gRPC; the LLM Gateway starts with the real model
# catalog and answers an authenticated request. No model provider is called.
set -euo pipefail

cd "$(dirname "$0")/.."
RAG_PORT="${RAG_PORT:-50051}"
API_PORT="${API_PORT:-8080}"
GATEWAY_PORT="${GATEWAY_PORT:-8090}"
DATABASE_URL="${DATABASE_URL:-postgres://enclave:enclave@localhost:5432/enclave?sslmode=disable}"
REDIS_URL="${REDIS_URL:-redis://localhost:6380/0}"
OBJECTSTORE_ENDPOINT="${OBJECTSTORE_ENDPOINT:-http://localhost:9000}"
OBJECTSTORE_ACCESS_KEY="${OBJECTSTORE_ACCESS_KEY:-enclave}"
OBJECTSTORE_SECRET_KEY="${OBJECTSTORE_SECRET_KEY:-enclave-dev-secret}"
# Throwaway secrets: nothing created by the smoke test outlives this run.
API_JWT_SIGNING_KEY="${API_JWT_SIGNING_KEY:-$(openssl rand -base64 32)}"
GATEWAY_TOKEN="smoke-$(openssl rand -hex 16)"
export RAG_SERVICE_TOKEN="smoke-$(openssl rand -hex 32)"
mkdir -p bin

pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

# wait_ok retries a curl command for up to 15s and prints the response body.
wait_ok() {
  local name=$1
  shift
  for _ in $(seq 1 30); do
    if body=$(curl -fsS "$@" 2>/dev/null); then
      echo "OK $name: $body"
      return 0
    fi
    sleep 0.5
  done
  echo "FAIL: $name did not answer within 15s" >&2
  return 1
}

# The RAG server connects to Postgres at startup; it calls the gateway only to answer, which
# the smoke test never asks it to do.
PYTHONPATH=src:gen RAG_GRPC_LISTEN="127.0.0.1:${RAG_PORT}" \
  DATABASE_URL="$DATABASE_URL" REDIS_URL="$REDIS_URL" RAG_GATEWAY_TOKEN="smoke-unused" \
  OBJECTSTORE_ENDPOINT="$OBJECTSTORE_ENDPOINT" OBJECTSTORE_ACCESS_KEY="$OBJECTSTORE_ACCESS_KEY" \
  OBJECTSTORE_SECRET_KEY="$OBJECTSTORE_SECRET_KEY" \
  uv run --quiet --directory rag python -m rag.main &
pids+=($!)

go build -o bin/api ./api/cmd/api
go build -o bin/gateway ./gateway/cmd/gateway

OBJECTSTORE_ENDPOINT="$OBJECTSTORE_ENDPOINT" OBJECTSTORE_ACCESS_KEY="$OBJECTSTORE_ACCESS_KEY" OBJECTSTORE_SECRET_KEY="$OBJECTSTORE_SECRET_KEY" \
  DATABASE_URL="$DATABASE_URL" REDIS_URL="$REDIS_URL" API_JWT_SIGNING_KEY="$API_JWT_SIGNING_KEY" API_ENABLE_DEBUG_ROUTES=true \
  API_HTTP_ADDR="127.0.0.1:${API_PORT}" RAG_GRPC_ADDR="127.0.0.1:${RAG_PORT}" ./bin/api &
pids+=($!)

# Provider keys are cleared so the smoke test never reaches a real provider.
NVIDIA_API_KEY="" ANTHROPIC_API_KEY="" DATABASE_URL="$DATABASE_URL" REDIS_URL="$REDIS_URL" \
  GATEWAY_HTTP_ADDR="127.0.0.1:${GATEWAY_PORT}" GATEWAY_SERVICE_TOKENS="smoke=$GATEWAY_TOKEN" ./bin/gateway &
pids+=($!)

wait_ok "api -> rag" "http://127.0.0.1:${API_PORT}/v1/debug/rag-ping"
wait_ok "gateway" -H "Authorization: Bearer $GATEWAY_TOKEN" "http://127.0.0.1:${GATEWAY_PORT}/v1/models"
