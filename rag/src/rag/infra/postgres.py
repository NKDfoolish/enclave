"""The RAG service's view of Postgres.

It reads documents, which the Core API owns, and writes document_index, which it owns. The
Core API stays the only writer of documents and permissions.
"""

from contextlib import contextmanager

import psycopg
from psycopg_pool import ConnectionPool

from rag.domain.documents import DocumentRecord, IndexState


class Documents:
    def __init__(self, pool: ConnectionPool):
        self._pool = pool

    @contextmanager
    def lock_index(self, document_id: str):
        # Multiple events for one document can reach different workers during a rename.
        # Hold one connection for the lock; sync uses the worker pool's other connection.
        with self._pool.connection() as conn:
            conn.execute("SELECT pg_advisory_xact_lock(hashtextextended(%s, 0))", (document_id,))
            yield

    def get(self, document_id: str) -> DocumentRecord | None:
        with self._pool.connection() as conn:
            row = conn.execute(
                """SELECT id::text, workspace_id::text, title, content_type, storage_key,
                          visibility, status, sha256
                   FROM documents WHERE id = %s""",
                (document_id,),
            ).fetchone()
        return DocumentRecord(*row) if row else None

    def index_state(self, document_id: str) -> IndexState | None:
        with self._pool.connection() as conn:
            row = conn.execute(
                """SELECT status, embedding_model, content_sha256, indexed_title
                   FROM document_index WHERE document_id = %s""",
                (document_id,),
            ).fetchone()
        return IndexState(*row) if row else None

    def save_index(
        self,
        document_id: str,
        *,
        status: str,
        embedding_model: str,
        content_sha256: bytes | None,
        indexed_title: str | None = None,
        chunk_count: int = 0,
        error: str | None = None,
    ) -> None:
        """Records the outcome. A document deleted meanwhile has no row to attach it to, which
        is fine: there is nothing left to report on."""
        try:
            with self._pool.connection() as conn:
                conn.execute(
                    """INSERT INTO document_index (document_id, status, embedding_model,
                           content_sha256, indexed_title, chunk_count, error)
                       VALUES (%s, %s, %s, %s, %s, %s, %s)
                       ON CONFLICT (document_id) DO UPDATE
                       SET status = EXCLUDED.status, embedding_model = EXCLUDED.embedding_model,
                           content_sha256 = EXCLUDED.content_sha256,
                           indexed_title = EXCLUDED.indexed_title,
                           chunk_count = EXCLUDED.chunk_count, error = EXCLUDED.error,
                           updated_at = now()""",
                    (
                        document_id,
                        status,
                        embedding_model,
                        content_sha256,
                        indexed_title,
                        chunk_count,
                        error,
                    ),
                )
        except psycopg.errors.ForeignKeyViolation:
            pass

    def visible_titles(
        self, workspace_id: str, document_ids: list[str], visibilities: list[str]
    ) -> dict[str, str]:
        """Titles of the documents, among document_ids, that the caller may read right now.

        Search filters on the copy of visibility stored with each vector, which can lag behind
        an unshare or a delete by a few seconds. This query is the authority: a passage whose
        document is missing here is dropped before it reaches the model.
        """
        if not document_ids:
            return {}
        with self._pool.connection() as conn:
            rows = conn.execute(
                """SELECT id::text, title FROM documents
                   WHERE workspace_id = %s AND id = ANY(%s::uuid[])
                     AND status = 'ready' AND visibility = ANY(%s)""",
                (workspace_id, document_ids, visibilities),
            ).fetchall()
        return dict(rows)
