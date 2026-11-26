"""The infra adapters against real Redis, Qdrant and Postgres."""

import time
import uuid
from pathlib import Path

import psycopg
import pytest
import redis
from psycopg_pool import ConnectionPool

from rag.domain.chunks import Chunk
from rag.infra.postgres import Documents
from rag.infra.queue import IngestQueue
from rag.infra.vectors import VectorStore
from tests.conftest import require_env

# --- Redis Streams queue ---


@pytest.fixture
def queues():
    [url] = require_env("TEST_REDIS_URL")
    client = redis.Redis.from_url(url)
    stream = f"test:ingest:{uuid.uuid4()}"
    yield lambda consumer, idle_ms=60_000: IngestQueue(client, consumer, stream, idle_ms)
    client.delete(stream, f"{stream}:dead")


def test_each_message_goes_to_one_worker_until_acknowledged(queues):
    first, second = queues("worker-1"), queues("worker-2")
    first.ensure_group()
    second.ensure_group()  # a second call is harmless (BUSYGROUP)
    first.publish("doc-1")

    [message] = first.receive(block_ms=100)
    assert message.document_id == "doc-1" and message.deliveries == 1
    assert second.receive(block_ms=100) == [], "a delivered message is not handed out twice"

    next_id = first.publish("doc-next")
    first.ack(message)
    assert [key for key, _ in first._redis.xrange(first._stream)] == [next_id]
    [remaining] = second.receive(block_ms=100)
    assert remaining.document_id == "doc-next"


def test_a_crashed_workers_message_is_claimed_by_another(queues):
    crashed, survivor = queues("crashed", idle_ms=50), queues("survivor", idle_ms=50)
    crashed.ensure_group()
    crashed.publish("doc-2")
    crashed.receive(block_ms=100)  # received, never acknowledged

    time.sleep(0.1)  # longer than the claim idle time
    [claimed] = survivor.receive(block_ms=100)

    assert claimed.document_id == "doc-2"
    assert claimed.deliveries == 2


def test_dead_letter_keeps_the_message_and_stops_redelivery(queues):
    worker = queues("worker", idle_ms=50)
    worker.ensure_group()
    worker.publish("doc-3")
    [message] = worker.receive(block_ms=100)

    worker.dead_letter(message, "gave up")
    time.sleep(0.1)

    assert worker.receive(block_ms=100) == []
    dead = worker._redis.xrange(f"{worker._stream}:dead")
    assert dead[0][1][b"document_id"] == b"doc-3"


# --- Qdrant ---


@pytest.fixture
def store():
    [url] = require_env("TEST_QDRANT_URL")
    store = VectorStore(url, f"test-{uuid.uuid4().hex[:12]}")
    store.ensure_collection(3)
    yield store
    store._client.delete_collection(store.collection)


def chunks(count: int) -> list[Chunk]:
    return [Chunk(i, f"chunk {i}", page=i + 1, heading="H") for i in range(count)]


def vectors(count: int) -> list[list[float]]:
    return [[1.0, float(i), 0.5] for i in range(count)]


def test_reindexing_overwrites_and_drops_leftover_chunks(store):
    store.replace_document("doc-a", "ws-1", "internal", chunks(5), vectors(5))
    store.replace_document("doc-a", "ws-1", "internal", chunks(5), vectors(5))
    assert store.count("doc-a") == 5, "the same chunks twice must not duplicate points"

    store.replace_document("doc-a", "ws-1", "internal", chunks(2), vectors(2))
    assert store.count("doc-a") == 2, "a shorter new version must not leave old chunks behind"


def test_search_filters_by_workspace_and_visibility(store):
    store.replace_document("doc-int", "ws-1", "internal", chunks(1), vectors(1))
    store.replace_document("doc-pub", "ws-1", "client_common", chunks(1), vectors(1))
    store.replace_document("doc-other", "ws-2", "client_common", chunks(1), vectors(1))
    query = [1.0, 0.0, 0.5]

    staff = store.search(query, "ws-1", ["internal", "client_common"], limit=10)
    client = store.search(query, "ws-1", ["client_common"], limit=10)

    assert {p.document_id for p in staff} == {"doc-int", "doc-pub"}
    assert {p.document_id for p in client} == {"doc-pub"}
    assert client[0].page == 1 and client[0].heading == "H" and client[0].score > 0.99


def test_visibility_change_and_delete(store):
    store.replace_document("doc-a", "ws-1", "client_common", chunks(2), vectors(2))
    query = [1.0, 0.0, 0.5]

    store.set_visibility("doc-a", "internal")
    assert store.search(query, "ws-1", ["client_common"], limit=10) == []

    store.delete_document("doc-a")
    assert store.count("doc-a") == 0


# --- Postgres ---


@pytest.fixture
def database():
    [url] = require_env("TEST_DATABASE_URL")
    pool = ConnectionPool(url, min_size=1, max_size=2, open=True)
    with pool.connection() as conn:
        workspace = conn.execute(
            "INSERT INTO workspaces (name) VALUES ('rag test') RETURNING id::text"
        ).fetchone()[0]
    yield pool, workspace
    with pool.connection() as conn:
        conn.execute("DELETE FROM workspaces WHERE id = %s", (workspace,))
    pool.close()


def insert_document(pool, workspace, title, visibility, status="ready") -> str:
    document_id = str(uuid.uuid4())
    with pool.connection() as conn:
        conn.execute(
            """INSERT INTO documents (id, workspace_id, title, filename, content_type,
                   size_bytes, sha256, storage_key, visibility, status)
               VALUES (%s, %s, %s, 'f.md', 'text/markdown', 10, '\\x01', %s, %s, %s)""",
            (document_id, workspace, title, f"k/{document_id}", visibility, status),
        )
    return document_id


def test_visible_titles_is_the_access_authority(database):
    pool, workspace = database
    documents = Documents(pool)
    shared = insert_document(pool, workspace, "Support", "client_common")
    internal = insert_document(pool, workspace, "Salaries", "internal")
    processing = insert_document(pool, workspace, "Draft", "client_common", status="processing")
    deleted = str(uuid.uuid4())

    ids = [shared, internal, processing, deleted]
    assert documents.visible_titles(workspace, ids, ["client_common"]) == {shared: "Support"}
    assert set(documents.visible_titles(workspace, ids, ["internal", "client_common"])) == {
        shared,
        internal,
    }
    assert documents.visible_titles(str(uuid.uuid4()), ids, ["client_common"]) == {}


def test_index_state_round_trip_and_deleted_documents(database):
    pool, workspace = database
    documents = Documents(pool)
    document_id = insert_document(pool, workspace, "Support", "client_common")

    record = documents.get(document_id)
    assert record.is_searchable and record.sha256 == b"\x01"

    documents.save_index(
        document_id,
        status="indexed",
        embedding_model="embed-local",
        content_sha256=record.sha256,
        indexed_title=record.title,
        chunk_count=4,
    )
    state = documents.index_state(document_id)
    assert state.indexed_title == record.title
    assert (state.status, state.embedding_model, state.content_sha256) == (
        "indexed",
        "embed-local",
        b"\x01",
    )

    with pool.connection() as conn:
        conn.execute("DELETE FROM documents WHERE id = %s", (document_id,))
    assert documents.get(document_id) is None
    assert documents.index_state(document_id) is None, "the index row goes with the document"
    # Recording a result for a document deleted meanwhile is not an error.
    documents.save_index(
        document_id, status="indexed", embedding_model="embed-local", content_sha256=None
    )


def test_psycopg_is_v3():
    assert psycopg.__version__.startswith("3.")


def test_upgrade_queues_existing_ready_documents(database):
    pool, workspace = database
    ready = insert_document(pool, workspace, "Existing", "internal")
    pending = insert_document(pool, workspace, "Uploading", "internal", status="uploading")
    migrations = Path(__file__).resolve().parents[2] / "migrations"
    migration = migrations / "00006_rag_provenance_and_backfill.sql"
    up = migration.read_text().split("-- +goose Down")[0]
    # Exercise the upgrade against pre-migration tables without changing the test schema.
    with pool.connection() as conn:
        conn.execute("CREATE TEMP TABLE conversation_messages () ON COMMIT DROP")
        conn.execute("CREATE TEMP TABLE document_index () ON COMMIT DROP")
        conn.execute("CREATE TEMP TABLE outbox_jobs (kind text, payload jsonb) ON COMMIT DROP")
        conn.execute(up)
        queued = conn.execute(
            "SELECT payload->>'document_id' FROM outbox_jobs "
            "WHERE kind = 'document.sync_search_index'"
        ).fetchall()
        assert (ready,) in queued
        assert (pending,) not in queued
