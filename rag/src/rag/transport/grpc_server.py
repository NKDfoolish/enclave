"""The RagService gRPC server, called by the Core API only."""

import hmac
import logging
from concurrent import futures

import grpc
from enclave.rag.v1 import rag_pb2, rag_pb2_grpc

from rag.app.answering import (
    Answerer,
    AnswerText,
    InvalidQuestion,
    Question,
    SourcesFound,
)
from rag.domain.answer import Turn
from rag.domain.documents import GatewayError

SERVICE_NAME = "rag"

log = logging.getLogger("rag.grpc")


class RagService(rag_pb2_grpc.RagServiceServicer):
    def __init__(self, answerer: Answerer | None, service_token: str):
        if not service_token:
            raise ValueError("empty RAG service token")
        self._authorization = ("Bearer " + service_token).encode()
        # None only in tests that exercise Ping alone.
        self._answerer = answerer

    def Ping(self, request: rag_pb2.PingRequest, context: grpc.ServicerContext):
        return rag_pb2.PingResponse(message=request.message, service=SERVICE_NAME)

    def Answer(self, request: rag_pb2.AnswerRequest, context: grpc.ServicerContext):
        authorization = dict(context.invocation_metadata()).get("authorization", "")
        if not hmac.compare_digest(authorization.encode(), self._authorization):
            context.abort(grpc.StatusCode.UNAUTHENTICATED, "invalid service credential")
        question = Question(
            workspace_id=request.workspace_id,
            visibilities=list(request.visibilities),
            text=request.question,
            history=[Turn(turn.role, turn.content) for turn in request.history],
            request_id=request.request_id,
        )
        events = self._answerer.answer(question)
        try:
            for event in events:
                if not context.is_active():
                    # The Core API cancelled (its caller hung up). Closing the generator
                    # closes the model stream, which tells the gateway to stop the provider.
                    log.info("answer %s cancelled by the caller", request.request_id)
                    return
                yield _to_proto(event)
        except InvalidQuestion as exc:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))
        except GatewayError as exc:
            # Not retryable means a setting blocks the call (e.g. the workspace allows no
            # provider for the model): the Core API reports it instead of retrying.
            code = (
                grpc.StatusCode.UNAVAILABLE
                if exc.retryable
                else grpc.StatusCode.FAILED_PRECONDITION
            )
            log.warning("answer %s: %s", request.request_id, exc)
            context.abort(code, str(exc))
        finally:
            events.close()


def _to_proto(event) -> rag_pb2.AnswerResponse:
    if isinstance(event, SourcesFound):
        return rag_pb2.AnswerResponse(
            sources=rag_pb2.Sources(
                sources=[
                    rag_pb2.Source(
                        number=source.number,
                        document_id=source.document_id,
                        title=source.title,
                        page=source.page,
                        section=source.section,
                        snippet=source.snippet,
                    )
                    for source in event.sources
                ]
            )
        )
    if isinstance(event, AnswerText):
        return rag_pb2.AnswerResponse(delta=event.text)
    return rag_pb2.AnswerResponse(done=rag_pb2.Done(cited=event.cited))


def build_server(
    addr: str, answerer: Answerer | None, service_token: str, max_workers: int = 10
) -> tuple[grpc.Server, int]:
    """Create a gRPC server bound to addr. Returns the server and the bound port."""
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=max_workers))
    rag_pb2_grpc.add_RagServiceServicer_to_server(RagService(answerer, service_token), server)
    port = server.add_insecure_port(addr)
    return server, port
