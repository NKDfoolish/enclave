package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"enclave/api/internal/document/domain"
	"enclave/internal/platform/objectstore"
	"enclave/internal/platform/outbox"
)

// Outbox job kinds handled by this context.
const (
	JobValidateUpload  = "document.validate_upload"
	JobDeleteObject    = "document.delete_object"
	JobSyncSearchIndex = "document.sync_search_index"
)

// abandonedUploadAge: an upload URL lives 15 minutes, so after an hour nobody can still be
// uploading to it.
const abandonedUploadAge = time.Hour

type validateUploadPayload struct {
	DocumentID uuid.UUID `json:"document_id"`
}

type deleteObjectPayload struct {
	StorageKey string `json:"storage_key"`
}

func deleteObjectJob(storageKey string) (outbox.Job, error) {
	return outbox.NewJob(JobDeleteObject, deleteObjectPayload{StorageKey: storageKey})
}

type syncSearchIndexPayload struct {
	DocumentID uuid.UUID `json:"document_id"`
}

// syncSearchIndexJob is queued with every change that affects search: a document becoming
// ready, a new title or visibility, a delete.
func syncSearchIndexJob(documentID uuid.UUID) (outbox.Job, error) {
	return outbox.NewJob(JobSyncSearchIndex, syncSearchIndexPayload{DocumentID: documentID})
}

// SyncSearchIndex is the outbox handler that hands a document change to the RAG service's
// ingestion queue (a Redis Stream). The outbox makes the hand-over reliable: if Redis is down
// when the change commits, the job is retried until the message is delivered. Delivering it
// twice is harmless, since the RAG service reconciles the index with the document's current
// state rather than replaying instructions.
func (s *Service) SyncSearchIndex(ctx context.Context, payload json.RawMessage) error {
	var p syncSearchIndexPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if err := s.searchIndex.RequestSync(ctx, p.DocumentID); err != nil {
		return fmt.Errorf("request search index sync for document %s: %w", p.DocumentID, err)
	}
	return nil
}

// ValidateUpload is the outbox handler that checks an uploaded file's content. Returning an
// error retries the job later; a file that is simply wrong marks the document failed instead.
// It is safe to run more than once.
func (s *Service) ValidateUpload(ctx context.Context, payload json.RawMessage) error {
	var p validateUploadPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	doc, err := s.repo.GetByID(ctx, p.DocumentID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil // deleted while waiting; nothing left to check
	}
	if err != nil {
		return err
	}
	if doc.Status != domain.StatusProcessing {
		return nil // already handled by an earlier run
	}

	data, err := s.readStoredFile(ctx, doc)
	if errors.Is(err, objectstore.ErrNotFound) {
		return s.reject(ctx, doc, "uploaded file is missing")
	}
	if err != nil {
		return err // storage hiccup: retry
	}
	if int64(len(data)) != doc.SizeBytes {
		return s.reject(ctx, doc, "uploaded file has a different size than declared")
	}
	if err := domain.ValidateContent(doc.ContentType, data); err != nil {
		return s.reject(ctx, doc, err.Error())
	}

	sum := sha256.Sum256(data)
	syncIndex, err := syncSearchIndexJob(doc.ID)
	if err != nil {
		return err
	}
	if err := s.repo.MarkReady(ctx, doc.ID, sum[:], syncIndex); err != nil {
		return fmt.Errorf("mark document %s ready: %w", doc.ID, err)
	}
	return nil
}

// readStoredFile reads at most maxBytes+1 bytes, so an oversized object cannot exhaust memory.
func (s *Service) readStoredFile(ctx context.Context, doc domain.Document) ([]byte, error) {
	body, err := s.store.Open(ctx, doc.StorageKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	data, err := io.ReadAll(io.LimitReader(body, s.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read stored file: %w", err)
	}
	return data, nil
}

// reject marks the document failed and queues deleting the bad file, in one transaction.
func (s *Service) reject(ctx context.Context, doc domain.Document, reason string) error {
	job, err := deleteObjectJob(doc.StorageKey)
	if err != nil {
		return err
	}
	if err := s.repo.MarkFailed(ctx, doc.ID, reason, job); err != nil {
		return fmt.Errorf("mark document %s failed: %w", doc.ID, err)
	}
	return nil
}

// DeleteObject is the outbox handler that removes a stored file. Deleting a missing file
// succeeds, so retries are safe.
func (s *Service) DeleteObject(ctx context.Context, payload json.RawMessage) error {
	var p deleteObjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	return s.store.Delete(ctx, p.StorageKey)
}

// CleanupAbandonedUploads deletes documents whose upload was started but never completed,
// together with any file that did arrive. It returns how many were removed.
func (s *Service) CleanupAbandonedUploads(ctx context.Context) (int, error) {
	docs, err := s.repo.AbandonedUploads(ctx, s.now().Add(-abandonedUploadAge), 100)
	if err != nil {
		return 0, fmt.Errorf("find abandoned uploads: %w", err)
	}

	removed := 0
	for _, doc := range docs {
		job, err := deleteObjectJob(doc.StorageKey)
		if err != nil {
			return removed, err
		}
		err = s.repo.DeleteAbandonedUpload(ctx, doc.ID, job)
		if errors.Is(err, domain.ErrNotFound) {
			continue // completed or deleted in the meantime
		}
		if err != nil {
			return removed, fmt.Errorf("delete abandoned upload %s: %w", doc.ID, err)
		}
		removed++
	}
	return removed, nil
}
