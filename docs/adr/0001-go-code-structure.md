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
api/internal/
├── <context>/            e.g. identity, workspace, document
│   ├── domain/           entities, value objects, domain errors, repository interfaces
│   ├── app/              use cases; depends only on domain
│   ├── infra/            Postgres etc.; implements domain interfaces
│   └── transport/        HTTP handlers; read request -> call app -> write response
└── platform/             shared plumbing with no business rules
    ├── config/  httpserver/  diagnostics/  ragclient/
```

Dependency rule: `transport -> app -> domain <- infra`. `domain` imports nothing from other layers.
`cmd/api` is the only place that wires concrete implementations together.

HTTP uses `net/http` with hand-written middleware (`platform/httpserver`) instead of a framework.

## Consequences

- A context's code lives in one folder; contexts communicate through their `app` APIs, not each
  other's tables or internals.
- Authorization rules live in `domain`/`app`, so the web API and MCP server enforce the same rules.
- Domain logic is testable without a database or HTTP server.
- Go forbids import cycles, which keeps the dependency rule honest.
- Small contexts may merge layers into fewer files; do not create empty folders ahead of need.
- Not adopted for now: domain events, CQRS, event sourcing. Revisit only when a concrete problem needs them.
