# Coding conventions

Code in this repo should be easy to read, review and change. When in doubt, choose the explicit,
boring option. These rules apply to every service; Go gets the most detail because most of the
backend is written in it.

## General

- **Explicit over clever.** A reader should understand a function top to bottom without jumping
  through layers of indirection.
- **No speculative abstraction.** Write concrete code first. Extract an interface, helper or
  generic only when a second real use appears (another implementation, a test fake, a repeated
  block). Three similar lines are better than a premature abstraction.
- **One job per function.** Keep control flow flat: handle errors and edge cases first, return
  early, and leave the happy path unindented.
- **Comments explain why**, not what the code already says. Document exported identifiers.
- **Names describe intent.** `pendingInvitations`, not `list2`; `expiresAt`, not `t`.

## Go

### Packages and layout

- Layout follows [ADR 0001](adr/0001-go-code-structure.md): bounded contexts under
  `api/internal/<context>/` with `domain/`, `app/`, `infra/`, `transport/` inside, and shared
  plumbing under `api/internal/platform/`.
- Package names are short, lowercase, singular nouns: `workspace`, `identity`, `httpserver`.
  Never `utils`, `common`, `helpers`, `models`, `base`.
- Dependencies point inward: `transport -> app -> domain <- infra`. `domain` imports no other
  layer. Contexts talk to each other through their `app` APIs, never through each other's tables.
- `cmd/api` is the only place that constructs concrete implementations and wires them together.

### Naming

| Rule | Good | Avoid |
|---|---|---|
| No stutter with the package name | `workspace.Service`, `workspace.Member` | `workspace.WorkspaceService` |
| Initialisms keep one case | `userID`, `HTTPAddr`, `ParseURL` | `userId`, `HttpAddr` |
| Receivers are 1–2 letters, consistent per type | `func (s *Service)` | `func (this *Service)`, `func (svc *Service)` |
| Scope decides length | `i`, `err`, `ctx` in short scopes; descriptive names in wide scopes | `theIndexOfLoop`, or `x` for a struct field |
| Getters have no `Get` prefix | `m.Role()` | `m.GetRole()` |
| Booleans read as questions | `isOwner`, `hasExpired`, `canInvite` | `owner`, `flag` |
| Constructors are `New` / `NewX` | `workspace.NewService(repo)` | `workspace.CreateServiceInstance` |
| Sentinel errors start with `Err` | `ErrNotFound`, `ErrForbidden` | `NotFoundError` for a value |

### Interfaces

- Small (one to three methods) and **declared by the consumer**, next to the code that uses them.
- Accept interfaces, return concrete structs.
- Only introduce one when there is a second implementation, a test needs a fake, or it is the
  `app` → `infra` boundary from ADR 0001 (the `Repository` port). No
  `IUserService` mirrors of every struct, no generic `Repository[T]`.

### Errors

- Errors are values: return them, do not panic for expected failures.
- Wrap with context using `%w`; messages are lowercase with no trailing punctuation:
  `fmt.Errorf("accept invitation %s: %w", invitationID, err)`.
- Compare with `errors.Is` / `errors.As`, never `==` on wrapped errors.
- Domain failures are sentinel errors in `domain` (`ErrForbidden`, `ErrNotMember`); `transport`
  maps them to HTTP status codes in one place.
- Handle every error or log it once at the boundary. Do not log and return the same error.

### Context, concurrency and resources

- `ctx context.Context` is the first parameter of anything that does I/O or may block.
- Every `(ctx, cancel)` pair is followed by `defer cancel()` where it is created.
- Anything that owns goroutines, connections or files has a `Close`, and its creator calls it.
- Never start a goroutine without knowing how it stops.

### Data access

- Hand-written SQL with pgx. No ORM, no query builders.
- Every query in a tenant-owned table filters by `workspace_id`.
- Transactions are explicit and short; commit or roll back in the same function.
- Schema changes only through goose migrations; never edit a migration that has been pushed.

### Things to avoid

- Global mutable state and `init()` side effects.
- Reflection, struct-tag magic and dependency-injection frameworks.
- Generics where a concrete type works.
- Boolean parameters that change behaviour (`Load(true)`); use two functions or an options struct.
- Functions longer than one screen, or nesting deeper than about three levels.

## HTTP API

- JSON field names are `snake_case`.
- Errors use one shape: `{"error": {"code": "...", "message": "...", "request_id": "..."}}`.
  `code` is stable for clients; `message` never contains internal details.
- Status codes: `400` invalid input, `401` not authenticated, `403` authenticated but not allowed
  inside a workspace the caller belongs to, `404` for resources in a workspace the caller cannot
  see (do not reveal that it exists), `409` conflicts, `502` downstream failure.
- Never log request bodies, query strings, passwords, tokens or document text.

## Python

- Type hints on every function signature; small functions; no metaprogramming.
- `ruff` decides formatting and import order.
- Same layering idea as Go when the service grows: `domain/`, `app/`, `infra/`, `transport/`.

## Tests

- Test behaviour, not implementation details. Table-driven tests for many similar cases.
- Test failure paths (forbidden, not found, timeout, invalid input) as carefully as the happy path.
- Fakes are small structs written by hand in the test file; no mocking frameworks.
- Integration tests talk to a real Postgres; unit tests do not touch the network.

## Commits

- Subject in the imperative, scoped by area: `api: add workspace invitations`, `ci: ...`, `docs: ...`.
- The body explains why the change is needed and how it was verified.
