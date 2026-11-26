import hashlib

import pytest

from rag.app.indexing import Indexer
from rag.domain.chunks import Section
from rag.domain.documents import DocumentRecord, GatewayError, IndexState, ParseError

CONTENT = b"# Refunds\nWithin 14 days.\n# Shipping\nThree days."
DIGEST = hashlib.sha256(CONTENT).digest()
DOC = "11111111-1111-1111-1111-111111111111"
WORKSPACE = "22222222-2222-2222-2222-222222222222"


def document(status="ready", visibility="internal", sha=DIGEST) -> DocumentRecord:
    return DocumentRecord(DOC, WORKSPACE, "Policy", "text/markdown", "key", visibility, status, sha)


class FakeDocuments:
    def __init__(self, *records: DocumentRecord | None, state: IndexState | None = None):
        # get() returns the records in turn (the last one repeats), so a test can change the
        # document between the first read and the check after writing.
        self.records = list(records)
        self.state = state
        self.saved: list[dict] = []

    def get(self, document_id):
        return self.records.pop(0) if len(self.records) > 1 else self.records[0]

    def index_state(self, document_id):
        return self.state

    def save_index(self, document_id, **fields):
        self.saved.append(fields)


class FakeFiles:
    def read(self, key):
        return CONTENT


class FakeEmbedder:
    embedding_model = "embed-test"

    def __init__(self, error: Exception | None = None):
        self.error = error
        self.calls: list[tuple] = []

    def embed(self, texts, input_type, workspace_id, request_id=""):
        self.calls.append((texts, input_type, workspace_id))
        if self.error:
            raise self.error
        return [[0.1, 0.2, 0.3] for _ in texts]


class FakeIndex:
    def __init__(self):
        self.points: dict[str, dict] = {}
        self.log: list[str] = []

    def ensure_collection(self, dimension):
        self.log.append(f"ensure {dimension}")

    def replace_document(self, document_id, workspace_id, visibility, chunks, vectors):
        self.log.append("replace")
        self.points[document_id] = {"visibility": visibility, "chunks": len(chunks)}

    def set_visibility(self, document_id, visibility):
        self.log.append(f"visibility {visibility}")
        if document_id in self.points:
            self.points[document_id]["visibility"] = visibility

    def delete_document(self, document_id):
        self.log.append("delete")
        self.points.pop(document_id, None)


def markdown_parser(content_type, data):
    from rag.infra.parsing import parse_document

    return parse_document(content_type, data)


def indexer(documents, embedder=None, parse=markdown_parser):
    index = FakeIndex()
    return Indexer(documents, FakeFiles(), parse, embedder or FakeEmbedder(), index), index


def test_ready_document_is_chunked_embedded_and_recorded():
    documents = FakeDocuments(document())
    embedder = FakeEmbedder()
    sut, index = indexer(documents, embedder)

    result = sut.sync(DOC)

    assert result.outcome == "indexed" and result.chunks == 2
    assert index.points[DOC] == {"visibility": "internal", "chunks": 2}
    texts, input_type, workspace = embedder.calls[0]
    assert input_type == "passage" and workspace == WORKSPACE
    assert texts[0] == "Policy > Refunds\n\nWithin 14 days."
    assert documents.saved[-1]["status"] == "indexed"
    assert documents.saved[-1]["embedding_model"] == "embed-test"


@pytest.mark.parametrize("record", [None, document(status="processing")])
def test_missing_or_unready_document_is_removed_from_the_index(record):
    sut, index = indexer(FakeDocuments(record))
    index.points[DOC] = {"visibility": "internal", "chunks": 3}

    assert sut.sync(DOC).outcome == "removed"
    assert DOC not in index.points


def test_unchanged_document_only_gets_its_visibility_refreshed():
    state = IndexState("indexed", "embed-test", DIGEST, "Policy")
    embedder = FakeEmbedder()
    sut, index = indexer(FakeDocuments(document(visibility="client_common"), state=state), embedder)
    index.points[DOC] = {"visibility": "internal", "chunks": 2}

    assert sut.sync(DOC).outcome == "refreshed"
    assert index.points[DOC]["visibility"] == "client_common"
    assert embedder.calls == [], "an unchanged file must not be embedded again"


@pytest.mark.parametrize(
    "state",
    [
        IndexState("indexed", "embed-other", DIGEST, "Policy"),  # the embedding model changed
        IndexState("indexed", "embed-test", b"v0", "Policy"),  # the file changed
        IndexState("indexed", "embed-test", DIGEST, "Old title"),
        IndexState("failed", "embed-test", DIGEST, "Policy"),  # the last attempt failed
    ],
)
def test_stale_or_failed_index_is_rebuilt(state):
    sut, _ = indexer(FakeDocuments(document(), state=state))

    assert sut.sync(DOC).outcome == "indexed"


def test_document_deleted_while_embedding_is_not_resurrected():
    # First read: ready. Read after writing the vectors: gone (deleted meanwhile).
    documents = FakeDocuments(document(), None)
    sut, index = indexer(documents)

    assert sut.sync(DOC).outcome == "removed"
    assert index.log[-2:] == ["replace", "delete"]
    assert DOC not in index.points
    assert documents.saved == []


def test_document_unshared_while_embedding_ends_with_the_new_visibility():
    documents = FakeDocuments(document(visibility="client_common"), document(visibility="internal"))
    sut, index = indexer(documents)

    assert sut.sync(DOC).outcome == "indexed"
    assert index.points[DOC]["visibility"] == "internal"


def test_changed_object_is_rejected_before_parsing():
    documents = FakeDocuments(document(sha=hashlib.sha256(b"original").digest()))
    sut, index = indexer(documents, parse=lambda *_: pytest.fail("parsed unverified bytes"))
    assert sut.sync(DOC).outcome == "failed"
    assert DOC not in index.points


def test_unreadable_file_is_marked_failed_without_retry():
    def broken_parser(content_type, data):
        raise ParseError("cannot read text/markdown")

    documents = FakeDocuments(document())
    sut, index = indexer(documents, parse=broken_parser)

    result = sut.sync(DOC)

    assert result.outcome == "failed" and "cannot read" in result.error
    assert documents.saved[-1]["status"] == "failed"


def test_document_without_text_is_marked_failed():
    documents = FakeDocuments(document())
    sut, _ = indexer(documents, parse=lambda content_type, data: [Section("   ")])

    assert sut.sync(DOC).outcome == "failed"
    assert "no text" in documents.saved[-1]["error"]


def test_retryable_gateway_error_is_raised_for_a_retry():
    sut, _ = indexer(FakeDocuments(document()), FakeEmbedder(GatewayError("503", retryable=True)))

    with pytest.raises(GatewayError):
        sut.sync(DOC)


def test_permanent_gateway_error_marks_the_document_failed():
    # E.g. the configured embedding model goes to a provider the workspace does not allow.
    documents = FakeDocuments(document())
    error = GatewayError("embed: gateway answered 403 provider_not_allowed", retryable=False)
    sut, _ = indexer(documents, FakeEmbedder(error))

    assert sut.sync(DOC).outcome == "failed"
    assert "provider_not_allowed" in documents.saved[-1]["error"]
