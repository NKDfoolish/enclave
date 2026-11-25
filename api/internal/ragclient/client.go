// Package ragclient is the Core API's gRPC client for the RAG service.
package ragclient

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ragv1 "enclave/gen/go/enclave/rag/v1"
)

// DefaultTimeout bounds each call so a slow RAG service cannot hang API requests.
const DefaultTimeout = 2 * time.Second

type Client struct {
	conn    *grpc.ClientConn
	rag     ragv1.RagServiceClient
	timeout time.Duration
}

// New creates a client for addr (host:port). The connection is established lazily on first call.
// Internal traffic is plaintext for local development; TLS comes with deployment (Part 9).
func New(addr string, opts ...grpc.DialOption) (*Client, error) {
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("create rag client for %s: %w", addr, err)
	}
	return &Client{conn: conn, rag: ragv1.NewRagServiceClient(conn), timeout: DefaultTimeout}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// Ping sends message to the RAG service and returns its echo.
func (c *Client) Ping(ctx context.Context, message string) (*ragv1.PingResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rag.Ping(ctx, &ragv1.PingRequest{Message: message})
	if err != nil {
		return nil, fmt.Errorf("rag ping: %w", err)
	}
	return resp, nil
}
