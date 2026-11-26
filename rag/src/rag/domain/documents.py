"""Documents as the RAG service sees them, and the errors its adapters raise."""

from dataclasses import dataclass


@dataclass(frozen=True)
class DocumentRecord:
    """A row of the Core API's documents table."""

    id: str
    workspace_id: str
    title: str
    content_type: str
    storage_key: str
    visibility: str
    status: str
    sha256: bytes | None

    @property
    def is_searchable(self) -> bool:
        """Only documents whose content passed the Core API's check are indexed."""
        return self.status == "ready"


@dataclass(frozen=True)
class IndexState:
    """What was last indexed for a document."""

    status: str  # "indexed" or "failed"
    embedding_model: str
    content_sha256: bytes | None
    indexed_title: str | None


class ParseError(Exception):
    """The file cannot be read as its declared type. Retrying will not help."""


class FileMissingError(Exception):
    """The stored file is gone, e.g. the document was deleted while the job waited."""


class GatewayError(Exception):
    """A model call through the LLM Gateway failed."""

    def __init__(self, message: str, retryable: bool):
        super().__init__(message)
        self.retryable = retryable
