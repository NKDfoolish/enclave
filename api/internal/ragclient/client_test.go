package ragclient

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	ragv1 "enclave/gen/go/enclave/rag/v1"
)

// fakeRag stands in for the Python RAG service so Go tests need no external process.
type fakeRag struct {
	ragv1.UnimplementedRagServiceServer
	delay time.Duration
}

func (f *fakeRag) Ping(ctx context.Context, req *ragv1.PingRequest) (*ragv1.PingResponse, error) {
	select {
	case <-time.After(f.delay):
		return &ragv1.PingResponse{Message: req.GetMessage(), Service: "fake-rag"}, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// newTestClient starts an in-memory gRPC server and returns a client connected to it.
func newTestClient(t *testing.T, srv ragv1.RagServiceServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	ragv1.RegisterRagServiceServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	c, err := New("passthrough:///bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPing(t *testing.T) {
	c := newTestClient(t, &fakeRag{})

	resp, err := c.Ping(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if resp.GetMessage() != "hello" || resp.GetService() != "fake-rag" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestPingTimesOut(t *testing.T) {
	c := newTestClient(t, &fakeRag{delay: time.Second})
	c.timeout = 50 * time.Millisecond

	_, err := c.Ping(context.Background(), "slow")
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}
