import threading

import grpc
import pytest
from enclave.rag.v1 import rag_pb2, rag_pb2_grpc

from rag.app.answering import AnswerDone, AnswerText, InvalidQuestion, SourcesFound
from rag.domain.answer import Source
from rag.domain.documents import GatewayError
from rag.transport.grpc_server import build_server

AUTH = (("authorization", "Bearer test-secret"),)


class FakeAnswerer:
    def __init__(self, events=(), error: Exception | None = None, endless: bool = False):
        self.events = list(events)
        self.error = error
        self.endless = endless
        self.question = None
        self.closed = threading.Event()

    def answer(self, question):
        self.question = question
        try:
            if self.error:
                raise self.error
            yield from self.events
            while self.endless:
                yield AnswerText("more ")
        finally:
            self.closed.set()


@pytest.fixture
def serve():
    servers = []

    def start(answerer=None):
        server, port = build_server("127.0.0.1:0", answerer, "test-secret")
        server.start()
        servers.append(server)
        channel = grpc.insecure_channel(f"127.0.0.1:{port}")
        return rag_pb2_grpc.RagServiceStub(channel)

    yield start
    for server in servers:
        server.stop(grace=None)


def ask(**fields) -> rag_pb2.AnswerRequest:
    request = rag_pb2.AnswerRequest(
        workspace_id="ws-1", visibilities=["client_common"], question="Refunds?", request_id="r1"
    )
    request.history.add(role="user", content="Hi")
    for key, value in fields.items():
        setattr(request, key, value)
    return request


def test_ping_echoes_message(serve):
    resp = serve().Ping(rag_pb2.PingRequest(message="hello"), timeout=2)

    assert resp.message == "hello"
    assert resp.service == "rag"


@pytest.mark.parametrize("metadata", [(), (("authorization", "Bearer wrong"),)])
def test_answer_rejects_untrusted_callers(serve, metadata):
    answerer = FakeAnswerer()
    with pytest.raises(grpc.RpcError) as raised:
        list(serve(answerer).Answer(ask(), metadata=metadata, timeout=2))
    assert raised.value.code() == grpc.StatusCode.UNAUTHENTICATED
    assert answerer.question is None


def test_answer_streams_events_in_order(serve):
    source = Source(1, "doc-a", "Support policy", 2, "Refunds", "Refunds take 14 days.")
    answerer = FakeAnswerer(
        [SourcesFound([source]), AnswerText("14 days "), AnswerText("[1]."), AnswerDone([1])]
    )

    responses = list(serve(answerer).Answer(ask(), metadata=AUTH, timeout=5))

    assert [r.WhichOneof("event") for r in responses] == ["sources", "delta", "delta", "done"]
    assert responses[0].sources.sources[0].title == "Support policy"
    assert responses[0].sources.sources[0].page == 2
    assert "".join(r.delta for r in responses[1:3]) == "14 days [1]."
    assert list(responses[3].done.cited) == [1]
    assert answerer.question.visibilities == ["client_common"]
    assert answerer.question.history[0].content == "Hi"


@pytest.mark.parametrize(
    ("error", "code"),
    [
        (InvalidQuestion("question must be 1-4000 characters"), grpc.StatusCode.INVALID_ARGUMENT),
        (GatewayError("chat: gateway answered 503", retryable=True), grpc.StatusCode.UNAVAILABLE),
        (
            GatewayError("chat: 403 provider_not_allowed", retryable=False),
            grpc.StatusCode.FAILED_PRECONDITION,
        ),
    ],
)
def test_errors_map_to_status_codes(serve, error, code):
    with pytest.raises(grpc.RpcError) as raised:
        list(serve(FakeAnswerer(error=error)).Answer(ask(), metadata=AUTH, timeout=5))

    assert raised.value.code() == code


def test_cancelling_the_call_stops_the_answer(serve):
    answerer = FakeAnswerer([SourcesFound([])], endless=True)
    call = serve(answerer).Answer(ask(), metadata=AUTH, timeout=10)

    next(call)  # sources arrived; the answer keeps streaming
    call.cancel()  # the Core API's caller hung up

    assert answerer.closed.wait(timeout=5), "the answer generator (and its model stream) must close"
