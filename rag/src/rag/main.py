"""Runs the RAG gRPC server: `python -m rag.main`."""

import logging
import os
import signal

from psycopg_pool import ConnectionPool

from rag import config as settings
from rag.app.answering import Answerer
from rag.infra.gateway import Gateway
from rag.infra.postgres import Documents
from rag.infra.vectors import VectorStore
from rag.transport.grpc_server import build_server

log = logging.getLogger("rag")


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    config = settings.load()

    service_token = os.environ.get("RAG_SERVICE_TOKEN")
    if not service_token:
        raise settings.ConfigError("RAG_SERVICE_TOKEN is required")

    pool = ConnectionPool(config.database_url, min_size=1, max_size=10, open=True)
    gateway = Gateway(
        config.gateway_url,
        config.gateway_token,
        config.embedding_model,
        config.chat_model,
        config.max_answer_tokens,
    )
    answerer = Answerer(
        embedder=gateway,
        chat=gateway,
        search=VectorStore(config.qdrant_url, config.embedding_model),
        access=Documents(pool),
        top_k=config.top_k,
        min_score=config.min_score,
    )

    server, port = build_server(config.grpc_listen, answerer, service_token)
    server.start()
    log.info(
        "rag gRPC server listening on port %d (embedding %s, chat %s)",
        port,
        config.embedding_model,
        config.chat_model,
    )

    # Stop gracefully on Ctrl+C or container stop, letting in-flight calls finish.
    def shutdown(signum, _frame):
        log.info("received signal %d, shutting down", signum)
        server.stop(grace=5)

    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)
    server.wait_for_termination()
    pool.close()


if __name__ == "__main__":
    main()
