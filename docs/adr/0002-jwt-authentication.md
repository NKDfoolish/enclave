# ADR 0002: Authentication with JWT access tokens, rotating refresh tokens and a Redis revocation list

- Status: accepted
- Date: 2026-11-07

## Context

The Core API needs to know who is calling on every request. Later services (RAG in Part 4, MCP
in Part 6, the agent in Part 7) also need to trust that identity without asking the Core API
each time.

Two approaches were considered:

1. **Server-side sessions**: a random token in a cookie, looked up in Postgres on every request.
   Simple and instantly revocable, but every service must query the session store.
2. **Signed JWTs**: the token itself proves identity, so any service holding the public key can
   verify it locally. Revocation needs extra state.

## Decision

Use signed JWTs with a revocation list.

- **Ed25519 (EdDSA) signatures.** Only the Core API holds the private key; other services verify
  with the public key and cannot mint tokens. The parser accepts EdDSA only, which blocks
  `alg=none` and HS256/public-key confusion attacks.
- **Two tokens per login**, both JWTs in `HttpOnly`, `SameSite=Lax` cookies:
  - access token, 15 minutes, `Path=/`, sent with every API call;
  - refresh token, 7 days, `Path=/v1/auth`, sent only to the auth endpoints.
- **Login families.** Every token from one login carries the same `fid`. Logout revokes the
  family, which kills both tokens and all their successors.
- **Refresh rotation with reuse detection.** `POST /v1/auth/refresh` retires the presented refresh
  token and issues a new pair. If a retired refresh token comes back more than 10 seconds later,
  it must be a copy, so the whole family is revoked. Within 10 seconds it is treated as two tabs
  racing and answered with `409 refresh_conflict` (the cookies are already fresh; retry).
- **Revocation list in Redis**, with a TTL on every key so it cleans itself up:
  - `auth:revoked-family:<fid>` lives as long as a refresh token can;
  - `auth:used-refresh:<jti>` lives until that refresh token would have expired, written with
    `SET NX GET` so concurrent refreshes cannot both win.
- **Fail closed.** If Redis cannot be read, requests are rejected rather than trusted.
- **Roles are not in the token.** Workspace roles are still read from Postgres on each request,
  so role changes and removals apply immediately even though the access token is still valid.

## Consequences

- Authenticating a request costs one signature check and one Redis `EXISTS`; no Postgres query.
- Not fully stateless: Redis is now required for the API to serve authenticated requests.
- A stolen access token works for at most 15 minutes unless the user logs out (which revokes it).
  A stolen refresh token is detected the first time both the thief and the user refresh.
- Downstream services can verify identity with the public key (Part 4). They must also check the
  revocation list, or accept the 15-minute window.
- The signing key is a secret: it has no default and the server refuses to start without it.
  Rotating it logs everyone out; key rotation with a `kid` header is deferred until needed.
