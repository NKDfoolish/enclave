#!/usr/bin/env bash
# Measures the latency the LLM Gateway adds, using k6 and a fake provider that answers
# instantly. Needs `make up` (Postgres, Redis) and k6 installed (brew install k6).
set -euo pipefail

cd "$(dirname "$0")/.."

GATEWAY_PORT=18090
PROVIDER_PORT=8099
TOKEN="bench-gateway-token-0123456789"
COMPOSE=$(docker compose version >/dev/null 2>&1 && echo "docker compose" || echo "docker-compose")
pids=()
workspace=""

cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  if [[ -n "$workspace" ]]; then
    # Deleting the workspace also deletes its usage rows (ON DELETE CASCADE).
    $COMPOSE exec -T postgres psql -U enclave -d enclave -qc "DELETE FROM workspaces WHERE id = '$workspace'" >/dev/null
  fi
}
trap cleanup EXIT

wait_for() {
  for _ in $(seq 1 60); do
    curl -sf "$1" >/dev/null && return 0
    sleep 0.5
  done
  echo "timed out waiting for $1" >&2
  return 1
}

go build -o bin/fakeprovider ./gateway/cmd/fakeprovider
go build -o bin/gateway ./gateway/cmd/gateway

FAKE_PROVIDER_ADDR=":$PROVIDER_PORT" ./bin/fakeprovider >/dev/null 2>&1 &
pids+=($!)

# A rate limit high enough never to trigger: this measures latency, not throttling.
GATEWAY_HTTP_ADDR=":$GATEWAY_PORT" \
GATEWAY_CATALOG_FILE=gateway/bench/catalog.json \
GATEWAY_SERVICE_TOKENS="bench=$TOKEN" \
GATEWAY_RATE_LIMIT_PER_MINUTE=10000000 \
GATEWAY_RATE_LIMIT_BURST=1000000 \
  ./bin/gateway >/dev/null 2>&1 &
pids+=($!)

wait_for "http://localhost:$GATEWAY_PORT/healthz"
workspace=$($COMPOSE exec -T postgres psql -U enclave -d enclave -tAc \
  "INSERT INTO workspaces (name) VALUES ('gateway benchmark') RETURNING id" | head -1 | tr -d '[:space:]')

k6 run --quiet \
  -e PROVIDER_URL="http://localhost:$PROVIDER_PORT" \
  -e GATEWAY_URL="http://localhost:$GATEWAY_PORT" \
  -e GATEWAY_TOKEN="$TOKEN" \
  -e WORKSPACE_ID="$workspace" \
  scripts/k6/gateway.js
