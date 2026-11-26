"""Queues every ready document for indexing: `make rag-reindex`.

Use it after changing RAG_EMBEDDING_MODEL. The worker compares each document's recorded
embedding model with the configured one, so documents already indexed with the current model
are only refreshed, not embedded again.
"""

import logging
import os

import psycopg
import redis

from rag.infra.queue import IngestQueue

log = logging.getLogger("rag.reindex")


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    with psycopg.connect(os.environ["DATABASE_URL"]) as conn:
        rows = conn.execute("SELECT id::text FROM documents WHERE status = 'ready'").fetchall()
    queue = IngestQueue(redis.Redis.from_url(os.environ["REDIS_URL"]))
    for (document_id,) in rows:
        queue.publish(document_id)
    log.info("queued %d documents", len(rows))


if __name__ == "__main__":
    main()
