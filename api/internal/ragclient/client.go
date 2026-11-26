// Package ragclient is the Core API's gRPC client for the RAG service.
package ragclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	ragv1 "enclave/gen/go/enclave/rag/v1"
)

// DefaultTimeout bounds each call so a slow RAG service cannot hang API requests.
const DefaultTimeout = 2 * time.Second

// DefaultAnswerTimeout bounds a whole streamed answer. Answers take long: a reasoning model
// can think for a minute before writing.
const DefaultAnswerTimeout = 3 * time.Minute

type Client struct {
	serviceToken  string
	conn          *grpc.ClientConn
	rag           ragv1.RagServiceClient
	timeout       time.Duration
	answerTimeout time.Duration
}

// New creates a client for addr (host:port). The connection is established lazily on first call.
// Internal traffic is plaintext for local development; TLS comes with deployment (Part 9).
func New(addr, serviceToken string, opts ...grpc.DialOption) (*Client, error) {
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("create rag client for %s: %w", addr, err)
	}
	return &Client{
		conn: conn, serviceToken: serviceToken,
		rag:           ragv1.NewRagServiceClient(conn),
		timeout:       DefaultTimeout,
		answerTimeout: DefaultAnswerTimeout,
	}, nil
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

// Answer streams an answer, calling onResponse for each event until the stream ends. If ctx
// is cancelled (the browser left), the call is cancelled and the RAG service stops the model.
func (c *Client) Answer(ctx context.Context, req *ragv1.AnswerRequest, onResponse func(*ragv1.AnswerResponse) error) error {
	ctx, cancel := context.WithTimeout(ctx, c.answerTimeout)
	defer cancel()

	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.serviceToken)
	stream, err := c.rag.Answer(ctx, req)
	if err != nil {
		return fmt.Errorf("rag answer: %w", err)
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("rag answer: %w", err)
		}
		if err := onResponse(resp); err != nil {
			return err
		}
	}
}
