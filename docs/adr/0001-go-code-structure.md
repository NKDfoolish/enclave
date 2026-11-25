# ADR 0001: Go code structure — bounded contexts outside, layers inside

- Status: accepted
- Date: 2026-10-11

## Context

The Core API will hold several business areas (identity, workspaces, documents) whose rules must be
shared by the web API (Parts 1–2) and the MCP server (Part 6). The project is also a learning
exercise in Domain-Driven Design and Clean Architecture, using the Go standard library for HTTP.

Two layouts were considered:

1. Layers at the top: `internal/{handlers,services,repositories}`.
2. Bounded contexts at the top, layers inside each one.

## Decision

Use option 2.

```text
api/                      Core API service
├── cmd/api/              composition root
└── internal/
    ├── <context>/        e.g. identity, workspace, document
    │   ├── domain/       entities, value objects, business rules, domain errors
    │   ├── app/          use cases + the Repository interface they consume
    │   ├── infra/        Postgres etc.; implements app's Repository
    │   └── transport/    HTTP handlers; read request -> call app -> write response
    └── config/ authn/ diagnostics/ ragclient/    plumbing only the Core API uses
gateway/                  LLM Gateway service (Part 3), same shape: cmd/gateway, internal/llm/...
internal/platform/        plumbing shared by services, no business rules:
                          httpserver, postgres, redis, env, testenv, objectstore, outbox, ...
migrations/               one schema history for the shared database
```

Dependency rule: `transport -> app -> domain <- infra`. The `Repository` interface is declared in
`app`, next to the code that consumes it (Go convention), and `infra` satisfies it implicitly; `app`
never imports `infra`. `domain` imports nothing from other layers.
Each service's `cmd/` package is the only place that wires concrete implementations together.

HTTP uses `net/http` with hand-written middleware (`internal/platform/httpserver`) instead of a
framework.

Go's `internal/` rule decides what is shared: code under `api/internal` can only be imported by the
Core API, code under the root `internal/` by every service in the module. A package moves to the
root when a second service needs it (Part 3 moved `httpserver`, `postgres`, `redis` and `testenv`
there, with the generic packages next to them, and added `env`).

## Consequences

- A context's code lives in one folder; contexts communicate through their `app` APIs, not each
  other's tables or internals. Example: `document/app` declares a one-method `Memberships`
  interface that `workspace/app.Service.MemberRole` satisfies, and uses `workspace/domain.Role`
  as the shared vocabulary for roles.
- `infra` may import types from its own context's `app` (e.g. a query struct used in the
  `Repository` interface); `app` still never imports `infra`.
- Authorization rules live in `domain`/`app`, so the web API and MCP server enforce the same rules.
- Domain logic is testable without a database or HTTP server.
- Go forbids import cycles, which keeps the dependency rule honest.
- Small contexts may merge layers into fewer files; do not create empty folders ahead of need.
- Not adopted for now: domain events, CQRS, event sourcing. Revisit only when a concrete problem needs them.
