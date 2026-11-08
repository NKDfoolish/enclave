# Part 2: Document library — Summary

Branch: `part-2-document-library`. Flow diagrams: [docs/flows.md](../flows.md) sections 13–20.
Decisions and trade-offs: [ADR 0003](../adr/0003-document-storage.md).

The plan's "Done when" criteria:

- [x] Staff can upload an internal document and explicitly share it with clients.
- [x] Clients can access client-common documents only. Deleting or unsharing a document blocks
  affected access immediately (new requests at once; an already issued download URL for at most
  60 s — see ADR 0003).

Tests: `api/cmd/api/documents_test.go` (12 subtests + pagination) against real Postgres, Redis
and object storage; `platform/outbox` tests including 4 concurrent workers on 20 jobs;
`document/domain` unit tests.

---

## 1. What was built

```text
api/internal/
├── document/                      NEW bounded context
│   ├── domain/   Document, Visibility, Status, rules (CanManage, IsVisibleTo),
│   │             file types + content validation (PDF / Markdown / DOCX), CleanFilename
│   ├── app/      service.go  CreateUpload, CompleteUpload, List, Get, DownloadURL, Update, Delete
│   │             jobs.go     ValidateUpload, DeleteObject, CleanupAbandonedUploads (background)
│   │             cursor.go   keyset pagination cursor
│   ├── infra/    Postgres repository (conditional status updates, outbox in the same tx)
│   └── transport/ HTTP handlers; /download answers with a 302
├── workspace/app  + MemberRole: how other contexts learn the caller's role
└── platform/
    ├── objectstore/ S3 client: presign PUT/GET, size, open, delete, bucket check
    ├── outbox/      Enqueue in a tx, Worker with SKIP LOCKED, retries, backoff, dead jobs
    └── testenv/     integration tests get Postgres/Redis/storage, skip locally, fail in CI
api/migrations/00003  documents, outbox_jobs
compose.yaml          objectstore (RustFS) + objectstore-init (bucket + CORS)
web/static            Documents panel: upload, status, share, download, delete, load more
```

New endpoints (all under `/v1/workspaces/{ws}/documents`):

| Method | Path | Who |
|---|---|---|
| `POST` | `` (create upload) | admin, staff |
| `POST` | `/{doc}/complete` | admin, staff |
| `GET` | `` (list, `?cursor&limit`) | members; clients see shared + ready only |
| `GET` | `/{doc}` | members who can see it |
| `GET` | `/{doc}/download` | members who can see it; 302 to a 60 s URL |
| `PATCH` | `/{doc}` (`title`, `visibility`) | admin, staff |
| `DELETE` | `/{doc}` | admin, staff |

## 2. Design decisions

| Decision | Why |
|---|---|
| Files in object storage, metadata and permissions in Postgres | Databases are bad at large blobs; storage is cheap and shared by every API instance. Postgres stays the source of truth for access. |
| S3 API via the AWS SDK; RustFS locally | Same code in production (Amazon S3). MinIO stopped publishing community images; RustFS passed a 17-point compatibility probe. |
| Presigned PUT bound to key, type and exact size, 15 min | The API never carries file bytes; the URL cannot be reused for another file, size or location. |
| Content checked by a background job before `ready` | The API never sees the bytes at upload time, so it must look afterwards; unchecked files can never be downloaded. |
| Storage key built from IDs only | A filename like `../../x` cannot choose where the file lands. |
| Presigned GET, 60 s, `attachment`, served from the storage origin | API does not stream files; a malicious file cannot script against the app's origin. Trade-off: up to 60 s window for an already issued URL. |
| Delete = delete row + enqueue file deletion in one transaction | Access is blocked at commit; the file is never forgotten even if storage is down (transactional outbox). |
| Outbox worker: `FOR UPDATE SKIP LOCKED`, claims only its own kinds | Several workers can share the table; each job runs on one worker at a time. |
| **Lease** instead of holding a transaction (`locked_until`, `lease_token`) | A slow handler (reading 25 MB, later embedding in Part 4) never pins a database connection, so user requests do not queue behind background work. A dead worker's job is retaken when its lease expires. |
| **Fencing token** on every finishing statement | A worker that stalled past its lease cannot delete or overwrite a job another worker now owns. |
| **Handler timeout (2 min) shorter than the lease (5 min) + heartbeat** | A hanging dependency cannot stall the worker or shutdown; long jobs keep their lease while alive. |
| Exponential backoff **with equal jitter**, dead after 10 claims | A broken dependency is not hammered, jobs that failed together spread their retries, and a stuck job waits for a human instead of retrying forever. `attempts` counts claims, so a job that crashes its worker still runs out. |
| Conditional status updates (`WHERE status = 'processing'`) | Retried or concurrent jobs cannot move a document backwards; handlers are idempotent. |
| Role checked before the document is looked up | A client gets 403 for any document ID, so the answer reveals nothing about which documents exist. |
| Keyset pagination with `(created_at, id)` | Stable pages while uploads arrive; uses an index instead of scanning skipped rows. |
| `make up` waits for the bucket setup container | `compose up --wait` does not wait for one-shot containers. |

## 3. Concepts worth knowing

- **Dual write**: two systems (Postgres and storage) cannot share one transaction. Choose the
  order so a crash leaves harmless garbage (an unreferenced file) rather than a broken record,
  and let a job clean up.
- **Transactional outbox**: write "what must happen next" in the same transaction as the change;
  a worker does it later, with retries. Handlers must be idempotent (at-least-once delivery).
- **`SELECT … FOR UPDATE SKIP LOCKED`**: a work queue in Postgres; locked rows are skipped instead
  of waited on.
- **Leases and fencing tokens**: hold work with a deadline written in the row rather than a
  database lock; renew it while alive (heartbeat); prove ownership with a token when finishing.
  Same idea as SQS visibility timeouts.
- **Jitter**: randomising retry delays prevents synchronised retry storms ("thundering herd").
- **Presigned URLs (SigV4)**: the server signs method, key, expiry and chosen headers with HMAC;
  storage recomputes the signature and rejects any change. The URL is a bearer credential.
- **CORS**: the browser asks storage (preflight `OPTIONS`) whether the app's origin may `PUT`;
  the bucket allows only the app's origins. CSP `connect-src` must also allow the storage origin.
- **Content sniffing**: trust bytes, not names or `Content-Type` headers.
- **Keyset vs OFFSET pagination**: `OFFSET` re-reads skipped rows and shifts when rows are added;
  a cursor continues from an exact position.

## 4. Things found while building

| Finding | Resolution |
|---|---|
| No MinIO image can be pulled any more (Docker Hub, Quay, Bitnami) | Probed RustFS 1.0.3 against every S3 feature we need; 17/17 passed. |
| AWS SDK modules release almost daily; the `credentials` module pulled newer core versions | Pinned SDK releases ≥ 2 weeks old; static credentials via `aws.CredentialsProviderFunc` instead of the extra module. |
| `compose up --wait` returned before the bucket existed | `make up` also runs `compose wait objectstore-init`. |
| Browser opened the UI as `127.0.0.1` but CORS allowed only `localhost` | Local CORS allows both origins. |
| A test expected 404 for a client renaming a document | 403 is correct: the role is checked first, for any ID, so nothing leaks; added a test with a random ID. |
| Mutation checks | Removing `SKIP LOCKED` ran 20 jobs 77 times; letting clients see internal documents, or forgetting the delete job, both failed the tests. After adding leases, removing each of lease check, fencing token, heartbeat, handler timeout and jitter failed its own test (5/5). |
| First lease version deadlocked: `defer cancel()` and `defer <-heartbeatDone` run in reverse order, so the wait ran before the cancel the heartbeat was waiting for | Cancel explicitly, then wait; covered by the heartbeat test. |

## 5. Deliberately left for later

- Workspace deletion must enqueue file deletions (no such feature yet).
- Virus scanning; document versions; folders; per-client sharing (after v1.0).
- Parsing, chunking and embedding documents — Part 4 extends the `processing` step.
- A separate worker process, if background work ever needs to scale apart from the API.
- Per-kind handler timeouts (Part 4 embedding jobs may need more than 2 minutes), `LISTEN/NOTIFY`
  to wake workers instantly, metrics and alerts on queue depth and dead jobs.
