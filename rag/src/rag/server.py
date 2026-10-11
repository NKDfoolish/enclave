from concurrent import futures

import grpc
from enclave.rag.v1 import rag_pb2, rag_pb2_grpc

SERVICE_NAME = "rag"


class RagService(rag_pb2_grpc.RagServiceServicer):
    def Ping(self, request: rag_pb2.PingRequest, context: grpc.ServicerContext):
        return rag_pb2.PingResponse(message=request.message, service=SERVICE_NAME)


def build_server(addr: str, max_workers: int = 10) -> tuple[grpc.Server, int]:
    """Create a gRPC server bound to addr. Returns the server and the bound port."""
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=max_workers))
    rag_pb2_grpc.add_RagServiceServicer_to_server(RagService(), server)
    port = server.add_insecure_port(addr)
    return server, port
