# Part 1: Workspace and access control — Summary

Branch: `part-1-workspace-access`.

Flow diagrams for this part: [docs/flows.md](../flows.md) sections 4–12.

The plan's "Done when" criteria:

- [x] An admin can invite members and change or revoke their roles (API + web UI).
- [x] Tests reject unauthorized operations and cross-workspace access, including guessed IDs
  (`api/cmd/api/routes_test.go`: integration subtests against real Postgres and Redis;
  `identity/app/tokens_test.go`: token unit tests with a fake clock).

---

## 1. What was built

```text
api/internal/
├── identity/                     register, login, refresh, logout (JWT, ADR 0002)
│   ├── domain/   User, errors
│   ├── app/      Service + Repository and RevocationStore ports, argon2id, JWT sign/verify
│   ├── infra/    users in Postgres, revocation list in Redis
│   └── transport/ HTTP handlers, token cookies, RequireAuth middleware
├── workspace/                    workspaces, members, roles, invitations
│   ├── domain/   Role + permission rules, Workspace, Member, Invitation, errors
│   ├── app/      Service (use cases, authorization) + Repository port
│   ├── infra/    Postgres repository (transactions, row locks)
│   └── transport/ HTTP handlers
└── platform/
    ├── authn/        Principal in the request context (identity writes, others read)
    ├── email/        address validation/normalisation
    ├── postgres/     pool + unique-violation helper
    ├── redis/        client + startup ping
    ├── securetoken/  random invitation tokens; only SHA-256 hashes are stored
    └── httpserver/   + ReadJSON (size limit, JSON-only, unknown fields rejected)
web/                  plain HTML/JS UI embedded in the binary, served at /
api/migrations/00002  users, workspaces, memberships, invitations
```

Request path for a workspace call:

```text
RequestID → AccessLog → Recover → mux → RequireAuth (access JWT: signature, expiry,
                                                    family not revoked in Redis → Principal)
  → workspace handler → workspace.Service: load caller's membership
      not a member        → ErrNotFound  → 404
      role not allowed    → ErrForbidden → 403
      allowed             → repository (SQL scoped by workspace_id) → 2xx
```

## 2. Design decisions

| Decision | Why |
|---|---|
| **JWT access (15 min) + rotating refresh token (7 days)**, see ADR 0002 | Identity is verified from the signature without a database query, and other services can verify it with the public key. |
| **Ed25519 signatures, parser accepts EdDSA only** | Only the Core API can mint tokens; blocks `alg=none` and algorithm-confusion attacks. |
| **Revocation list in Redis** (revoked login families, used refresh tokens), keys with TTL | Logout works instantly; the list cleans itself up as tokens expire. |
| **Refresh reuse detection** with a 10 s grace window | A copied refresh token revokes the whole login; two tabs refreshing at once do not. |
| **Fail closed** when Redis is unreachable | A token is never trusted without checking the revocation list. |
| Refresh cookie scoped to `Path=/v1/auth` | The long-lived credential is not sent with ordinary API calls. |
| Cookies `HttpOnly`, `SameSite=Lax`, `Secure` (configurable) | JS cannot read it (XSS); browsers do not send it on cross-site POST/PATCH/DELETE (CSRF). |
| `ReadJSON` requires `Content-Type: application/json` | A cross-site HTML form cannot send that header, so it is a second CSRF barrier. |
| **argon2id** with OWASP parameters, PHC string format | Memory-hard password hashing; parameters stored with the hash so they can be raised later. |
| Dummy hash check on unknown email | Login takes the same time whether or not the email exists, so timing does not reveal accounts. |
| **Role read from the DB on every request**, never put in the JWT | Role changes and removals apply to the very next request, even with a valid token. |
| **Non-members get 404**, members without permission get 403 | Outsiders cannot learn whether a workspace exists; malformed IDs also get 404. |
| Every tenant query filters by `workspace_id` | Even with a valid ID from another workspace, rows never cross tenants. |
| Workspace row lock (`SELECT … FOR UPDATE`) around role changes | Two admins demoting each other concurrently cannot leave zero admins. |
| Accepting an invitation is one `UPDATE … WHERE still open RETURNING` + insert in a transaction | Two concurrent accepts of the same link cannot both succeed. |
| Invitation token in the URL fragment (`/#accept=…`) | Fragments are never sent to the server, so tokens stay out of access logs. |
| Partial unique index: one **open** invitation per email per workspace | Expired ones are closed before inserting, because `now()` cannot appear in an index predicate. |
| `Repository` interfaces live in `app`, implemented by `infra` | Keeps use cases independent of Postgres (ADR 0001, updated). |
| Integration tests use a separate `enclave_test` DB, Redis db 15 and unique data per test | Packages can run in parallel without truncating each other's data. In CI a missing DB fails instead of skipping. |

## 3. Concepts worth knowing

- **Authentication vs authorization**: identity answers "who are you" (token → user);
  workspace answers "what may you do here" (membership → role → rule).
- **RBAC scoped to a tenant**: the role lives on the membership, not on the user.
- **Deny by default**: every use case starts by loading the caller's membership; access exists
  only where a rule allows it.
- **Check-then-act races**: reading state and acting on it must happen in one transaction with
  the right lock or a conditional `UPDATE`, otherwise concurrent requests break invariants.
- **Uniform failure responses**: wrong password and unknown email return the same
  error code and take similar time.
- **Stateless vs revocable**: a pure JWT cannot be taken back before it expires. Short access
  tokens plus a small revocation list buy back logout at the cost of one Redis lookup.
- **Salt**: 16 random bytes per password, stored in the hash string, so equal passwords hash
  differently and precomputed tables are useless.

## 4. Bugs found while building

| Bug | Fix |
|---|---|
| `go get` bumped the module to Go 1.26 because the newest `x/crypto` requires it | Pinned `x/crypto v0.55.0`, the newest release supporting Go 1.25; a toolchain upgrade deserves its own commit. |
| `"GET /"` and `"/v1/"` conflict under Go 1.22 pattern rules (each more specific in one dimension) | Register the UI at `"/"` and let `web.Handler` reject non-GET methods. |
| `gosec` G115: `uint32(len(key))` may overflow | Accept only hashes with the 32-byte key length we produce. |
| Pasting an invite link into an open tab did nothing | A fragment change does not reload the page; the UI now listens for `hashchange`. |
| `go-redis v9.23` requires Go 1.26 | Pinned `v9.22.0`. |
| Re-running an edited, already-applied migration left the dropped `sessions` table behind | `down` runs the new Down section, not the old one; cleaned the local databases by hand. This is why pushed migrations are never edited. |

## 5. Deliberately left for later

- **Login rate limiting / lockout** — needs Redis counters (Part 3).
- **Email delivery** for invitations — the admin copies the link for now.
- Password reset, changing email or password (both should revoke all login families).
- Signing-key rotation with a `kid` header.
- **Downstream services verifying tokens** with the public key (Part 4).
