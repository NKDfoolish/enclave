"""Keeping the search index in step with the documents table.

sync() does not take an instruction ("index this", "delete that"). It looks at the document
as Postgres has it now and makes Qdrant match: index it, refresh its visibility, or remove it.
Messages can therefore arrive twice, late or out of order, and the result is still right. A
delete that races an indexing run is caught by checking Postgres again after writing.
"""

import hashlib
import logging
from collections.abc import Callable
from dataclasses import dataclass
from typing import Protocol

from rag.domain.chunks import Chunk, Section, chunk_sections, embedding_text
from rag.domain.documents import (
    DocumentRecord,
    FileMissingError,
    GatewayError,
    IndexState,
    ParseError,
)

log = logging.getLogger("rag.indexing")


class DocumentSource(Protocol):
    def get(self, document_id: str) -> DocumentRecord | None: ...
    def index_state(self, document_id: str) -> IndexState | None: ...
    def save_index(self, document_id: str, **fields) -> None: ...


class FileSource(Protocol):
    def read(self, key: str) -> bytes: ...


# Turns a file's bytes into sections; raises ParseError for unreadable files.
Parser = Callable[[str, bytes], list[Section]]


class Embedder(Protocol):
    embedding_model: str

    def embed(
        self, texts: list[str], input_type: str, workspace_id: str, request_id: str = ""
    ) -> list[list[float]]: ...


class ChunkIndex(Protocol):
    def ensure_collection(self, dimension: int) -> None: ...
    def replace_document(
        self, document_id: str, workspace_id: str, visibility: str, chunks: list[Chunk], vectors
    ) -> None: ...
    def set_visibility(self, document_id: str, visibility: str) -> None: ...
    def delete_document(self, document_id: str) -> None: ...


@dataclass(frozen=True)
class SyncResult:
    # "indexed", "refreshed" (only visibility changed), "removed" or "failed".
    outcome: str
    chunks: int = 0
    error: str = ""


class Indexer:
    def __init__(
        self,
        documents: DocumentSource,
        files: FileSource,
        parse: Parser,
        embedder: Embedder,
        index: ChunkIndex,
    ):
        self._documents = documents
        self._files = files
        self._parse = parse
        self._embedder = embedder
        self._index = index

    def sync(self, document_id: str) -> SyncResult:
        """Brings the index in line with the document. Raises on failures worth retrying (the
        gateway or a store is down); records and returns permanent failures."""
        document = self._documents.get(document_id)
        if document is None or not document.is_searchable:
            self._index.delete_document(document_id)
            return SyncResult("removed")

        state = self._documents.index_state(document_id)
        if self._is_current(state, document):
            # The embedding inputs are unchanged; refresh the visibility filter.
            self._index.set_visibility(document_id, document.visibility)
            return SyncResult("refreshed")

        try:
            chunks = self._chunks(document)
            if not chunks:
                return self._fail(document, "no text found (a scanned PDF needs OCR)")
            vectors = self._embedder.embed(
                [embedding_text(document.title, chunk) for chunk in chunks],
                input_type="passage",
                workspace_id=document.workspace_id,
            )
        except ParseError as exc:
            return self._fail(document, str(exc))
        except FileMissingError:
            # Deleted while waiting; the delete's own message removes anything left.
            return SyncResult("removed")
        except GatewayError as exc:
            if exc.retryable:
                raise
            return self._fail(document, str(exc))

        self._index.ensure_collection(len(vectors[0]))
        self._index.replace_document(
            document_id, document.workspace_id, document.visibility, chunks, vectors
        )

        # The document may have been deleted or unshared while it was being embedded. Writing
        # first and checking after means a stale run can never leave it searchable.
        latest = self._documents.get(document_id)
        if latest is None or not latest.is_searchable:
            self._index.delete_document(document_id)
            return SyncResult("removed")
        if latest.visibility != document.visibility:
            self._index.set_visibility(document_id, latest.visibility)

        self._documents.save_index(
            document_id,
            status="indexed",
            embedding_model=self._embedder.embedding_model,
            content_sha256=document.sha256,
            indexed_title=document.title,
            chunk_count=len(chunks),
        )
        log.info("indexed document %s: %d chunks", document_id, len(chunks))
        return SyncResult("indexed", chunks=len(chunks))

    def record_failure(self, document_id: str, error: str) -> None:
        """Marks a document failed after its message was given up on."""
        document = self._documents.get(document_id)
        if document is not None:
            self._fail(document, error)

    def _is_current(self, state: IndexState | None, document: DocumentRecord) -> bool:
        return (
            state is not None
            and state.status == "indexed"
            and state.embedding_model == self._embedder.embedding_model
            and state.content_sha256 == document.sha256
            and state.indexed_title == document.title
        )

    def _chunks(self, document: DocumentRecord) -> list[Chunk]:
        data = self._files.read(document.storage_key)
        if hashlib.sha256(data).digest() != document.sha256:
            raise ParseError("stored file differs from validated upload")
        return chunk_sections(self._parse(document.content_type, data))

    def _fail(self, document: DocumentRecord, error: str) -> SyncResult:
        log.warning("cannot index document %s: %s", document.id, error)
        self._index.delete_document(document.id)
        self._documents.save_index(
            document.id,
            status="failed",
            embedding_model=self._embedder.embedding_model,
            content_sha256=document.sha256,
            error=error[:500],
        )
        return SyncResult("failed", error=error)
