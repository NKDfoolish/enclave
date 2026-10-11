import grpc
import pytest
from enclave.rag.v1 import rag_pb2, rag_pb2_grpc

from rag.server import build_server


@pytest.fixture
def stub():
    server, port = build_server("127.0.0.1:0")
    server.start()
    channel = grpc.insecure_channel(f"127.0.0.1:{port}")
    yield rag_pb2_grpc.RagServiceStub(channel)
    channel.close()
    server.stop(grace=None)


def test_ping_echoes_message(stub):
    resp = stub.Ping(rag_pb2.PingRequest(message="hello"), timeout=2)

    assert resp.message == "hello"
    assert resp.service == "rag"
