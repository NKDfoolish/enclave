import logging
import os
import signal

from rag.server import build_server

log = logging.getLogger("rag")


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    addr = os.environ.get("RAG_GRPC_LISTEN", "0.0.0.0:50051")

    server, port = build_server(addr)
    server.start()
    log.info("rag gRPC server listening on port %d", port)

    # Stop gracefully on Ctrl+C or container stop, letting in-flight calls finish.
    def shutdown(signum, _frame):
        log.info("received signal %d, shutting down", signum)
        server.stop(grace=5)

    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)
    server.wait_for_termination()


if __name__ == "__main__":
    main()
