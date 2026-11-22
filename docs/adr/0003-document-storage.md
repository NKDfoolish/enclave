# ADR 0003: Document storage with presigned URLs and a transactional outbox

- Status: accepted
- Date: 2026-11-08

## Context

Part 2 stores users' original files (PDF, Markdown, DOCX, up to 25 MB) and must:

- keep files private, with access checked against the current workspace role and visibility;
- block access immediately when a document is unshared or deleted;
- never keep the file of a deleted document;
- never serve a file that is not what it claims to be.

Flows: [docs/flows.md](../flows.md), sections 13–20.

## Decision

**Object storage, S3 API.** Files live in a private bucket; metadata and permissions live in
Postgres, which stays the source of truth. The code uses the AWS SDK for Go against any
S3-compatible server: Amazon S3 in production, RustFS locally and in CI. MinIO was the first
choice, but it no longer publishes community container images; RustFS passed a compatibility
probe for every feature used here (presigned PUT with signed size and type, presigned GET with
response header overrides, expiry, CORS, delete).

**Uploads bypass the API.** `POST /documents` registers the document (`uploading`) and returns a
presigned PUT valid for 15 minutes that is bound to one server-generated key, the declared
`Content-Type` and the exact `Content-Length`. The browser uploads directly to storage, then calls
`/complete`; the API checks the object exists with the declared size, sets `processing` and
queues a content check. Only documents that pass the check become `ready`, and only `ready`
documents can be downloaded.

**Content is checked from the bytes**, not the name: PDF header, valid UTF-8 without NUL bytes
for Markdown, ZIP containing `word/document.xml` for DOCX (directory only, nothing is
decompressed). Rejected files are marked `failed` and deleted.

**Downloads are presigned GETs.** `/download` checks access, then redirects (302) to a 60-second
URL that forces `Content-Disposition: attachment` with a sanitised filename and the verified
content type. Files are served from the storage origin, never the app's, so even a malicious
file cannot run script against the app's cookies.

**Transactional outbox for follow-up work.** Deleting a document, rejecting a file and finishing
an upload each write an `outbox_jobs` row in the same transaction as the change. A worker
goroutine **leases** a due job in one short statement (`locked_until`, a random `lease_token`,
`FOR UPDATE SKIP LOCKED`) and commits, then runs the idempotent handler outside any transaction
with a 2-minute timeout while renewing the 5-minute lease every third of it. Finishing (delete
on success, reschedule or mark dead on failure) requires the lease token, so a worker that lost
its lease cannot touch the job. Retries back off exponentially (2 s doubling, capped at 1 h)
with half of each delay random ("equal jitter") so jobs that failed together do not retry
together; a job is parked as dead after 10 claims. A ticker removes uploads abandoned for over
an hour.

**Keyset pagination** for listing: an opaque cursor holding `(created_at, id)` of the last row.

## Consequences

- The API never buffers or streams user files, so its memory and bandwidth do not grow with
  upload size. The content check reads at most `limit + 1` bytes per job.
- **Accepted deviation from "block access immediately":** every new download request is checked
  against the database, but a presigned URL handed out just before an unshare stays valid for at
  most 60 seconds. Streaming downloads through the API would close that window at the cost of
  moving file bytes through the API; revisit if this becomes a requirement.
- Uploading is three requests instead of one, and storage needs CORS for the app's origin. The
  presigned URL is a bearer credential for its lifetime and must not be logged.
- Deleting a document blocks access at once (row deleted); the stored file follows within
  seconds, or later if storage is down, but is never forgotten.
- Outbox handlers must be idempotent: a job can run twice if the process stops after the handler
  succeeds but before the job row is deleted, or if a lease expires while a handler is still
  running (the 2-minute timeout under a 5-minute lease makes this rare).
- A job whose worker crashes is picked up again only after its lease expires (up to 5 minutes).
- Deleting a whole workspace would cascade document rows without queueing file deletion. No such
  feature exists yet; it must enqueue `delete_object` jobs when it is added.
- Not done yet: virus scanning, document versions, per-client sharing (planned after v1.0).
