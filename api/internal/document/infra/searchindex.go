package infra

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// IngestStream is the Redis Stream the RAG service's ingestion workers read
// (rag/src/rag/infra/queue.py). Each message is {"document_id": "<uuid>"}.
const IngestStream = "rag:ingest"

// SearchIndex publishes document changes to the RAG service's ingestion queue.
type SearchIndex struct {
	client *goredis.Client
}

func NewSearchIndex(client *goredis.Client) *SearchIndex {
	return &SearchIndex{client: client}
}

func (s *SearchIndex) RequestSync(ctx context.Context, documentID uuid.UUID) error {
	err := s.client.XAdd(ctx, &goredis.XAddArgs{
		Stream: IngestStream,
		Values: map[string]any{"document_id": documentID.String()},
	}).Err()
	if err != nil {
		return fmt.Errorf("add to %s: %w", IngestStream, err)
	}
	return nil
}
