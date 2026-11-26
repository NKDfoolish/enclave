"""Runs the ingestion worker: `python -m rag.worker`.

It takes document IDs from the Redis Stream and brings the search index in line with each
document (Indexer.sync). A message is acknowledged once handled; a failure worth retrying
leaves it pending, and Redis hands it out again after CLAIM_IDLE_MS. After MAX_DELIVERIES the
document is marked failed and the message moved to the dead-letter stream.

Several workers can run at once: the consumer group gives each message to one of them.
"""

import logging
import signal

import redis
from psycopg_pool import ConnectionPool

from rag import config as settings
from rag.app.indexing import Indexer
from rag.infra.gateway import Gateway
from rag.infra.parsing import parse_document
from rag.infra.postgres import Documents
from rag.infra.queue import MAX_DELIVERIES, IngestQueue, Message
from rag.infra.storage import ObjectStore
from rag.infra.vectors import VectorStore

log = logging.getLogger("rag.worker")


def handle(indexer: Indexer, queue: IngestQueue, message: Message) -> None:
    """Processes one message. Never raises: every outcome ends in ack, dead letter, or a
    pending message that will be delivered again."""
    try:
        result = indexer.sync(message.document_id)
    except Exception as exc:  # retryable: gateway, Qdrant, storage or Postgres trouble
        if message.deliveries >= MAX_DELIVERIES:
            log.error("giving up on document %s: %s", message.document_id, exc)
            indexer.record_failure(message.document_id, f"gave up after retries: {exc}")
            queue.dead_letter(message, str(exc))
        else:
            log.warning(
                "document %s failed (delivery %d of %d), will retry: %s",
                message.document_id,
                message.deliveries,
                MAX_DELIVERIES,
                exc,
            )
        return
    log.info("document %s: %s", message.document_id, result.outcome)
    queue.ack(message)


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    config = settings.load()

    pool = ConnectionPool(config.database_url, min_size=1, max_size=2, open=True)
    queue = IngestQueue(redis.Redis.from_url(config.redis_url))
    queue.ensure_group()
    documents = Documents(pool)
    indexer = Indexer(
        documents=documents,
        files=ObjectStore(
            config.objectstore_endpoint,
            config.objectstore_bucket,
            config.objectstore_access_key,
            config.objectstore_secret_key,
            config.objectstore_region,
        ),
        parse=parse_document,
        embedder=Gateway(
            config.gateway_url,
            config.gateway_token,
            config.embedding_model,
            config.chat_model,
            config.max_answer_tokens,
        ),
        index=VectorStore(config.qdrant_url, config.embedding_model),
    )

    stopping = False

    def stop(signum, _frame):
        nonlocal stopping
        log.info("received signal %d, stopping after the current document", signum)
        stopping = True

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)

    log.info("ingestion worker %s started (embedding %s)", queue.consumer, config.embedding_model)
    while not stopping:
        for message in queue.receive(block_ms=2000):
            with documents.lock_index(message.document_id):
                handle(indexer, queue, message)
    pool.close()
    log.info("ingestion worker stopped")


if __name__ == "__main__":
    main()
