// Package infra stores document metadata in Postgres. Every query on a workspace's documents
// filters by workspace_id. Status changes are conditional updates, so a stale or repeated
// request cannot move a document backwards.
package infra

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enclave/api/internal/document/app"
	"enclave/api/internal/document/domain"
	"enclave/api/internal/platform/outbox"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

const documentColumns = `id, workspace_id, title, filename, content_type, size_bytes, sha256,
	storage_key, visibility, status, failure_reason, uploaded_by, created_at, updated_at`

func scanDocument(row pgx.CollectableRow) (domain.Document, error) {
	var (
		doc           domain.Document
		failureReason *string
		uploadedBy    uuid.NullUUID // NULL when the uploader's account was deleted
	)
	err := row.Scan(&doc.ID, &doc.WorkspaceID, &doc.Title, &doc.Filename, &doc.ContentType,
		&doc.SizeBytes, &doc.SHA256, &doc.StorageKey, &doc.Visibility, &doc.Status,
		&failureReason, &uploadedBy, &doc.CreatedAt, &doc.UpdatedAt)
	if failureReason != nil {
		doc.FailureReason = *failureReason
	}
	doc.UploadedBy = uploadedBy.UUID
	return doc, err
}

func (r *Repository) queryOne(ctx context.Context, query string, args ...any) (domain.Document, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return domain.Document{}, fmt.Errorf("query document: %w", err)
	}
	doc, err := pgx.CollectExactlyOneRow(rows, scanDocument)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Document{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Document{}, fmt.Errorf("scan document: %w", err)
	}
	return doc, nil
}

func (r *Repository) Create(ctx context.Context, doc domain.Document) (domain.Document, error) {
	return r.queryOne(ctx, `
		INSERT INTO documents (id, workspace_id, title, filename, content_type, size_bytes,
		                       storage_key, visibility, status, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+documentColumns,
		doc.ID, doc.WorkspaceID, doc.Title, doc.Filename, doc.ContentType, doc.SizeBytes,
		doc.StorageKey, doc.Visibility, doc.Status, doc.UploadedBy,
	)
}

func (r *Repository) Get(ctx context.Context, workspaceID, documentID uuid.UUID) (domain.Document, error) {
	return r.queryOne(ctx,
		`SELECT `+documentColumns+` FROM documents WHERE workspace_id = $1 AND id = $2`,
		workspaceID, documentID)
}

// GetByID is for background jobs, which act on a document they were given, not on behalf
// of a user in a workspace.
func (r *Repository) GetByID(ctx context.Context, documentID uuid.UUID) (domain.Document, error) {
	return r.queryOne(ctx, `SELECT `+documentColumns+` FROM documents WHERE id = $1`, documentID)
}

// List returns one page in (created_at DESC, id DESC) order. The row comparison
// (created_at, id) < (cursor) continues exactly after the previous page and uses the
// documents_workspace_page_idx index.
func (r *Repository) List(ctx context.Context, q app.ListQuery) ([]domain.Document, error) {
	var afterTime *time.Time
	afterID := uuid.Nil
	if q.After != nil {
		afterTime, afterID = &q.After.CreatedAt, q.After.ID
	}

	rows, err := r.pool.Query(ctx, `
		SELECT `+documentColumns+`
		FROM documents
		WHERE workspace_id = $1
		  AND (NOT $2 OR (visibility = 'client_common' AND status = 'ready'))
		  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4))
		ORDER BY created_at DESC, id DESC
		LIMIT $5`,
		q.WorkspaceID, q.SharedReadyOnly, afterTime, afterID, q.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select documents: %w", err)
	}
	docs, err := pgx.CollectRows(rows, scanDocument)
	if err != nil {
		return nil, fmt.Errorf("scan documents: %w", err)
	}
	return docs, nil
}

func (r *Repository) UpdateDetails(ctx context.Context, workspaceID, documentID uuid.UUID, title string, visibility domain.Visibility) (domain.Document, error) {
	return r.queryOne(ctx, `
		UPDATE documents
		SET title = $3, visibility = $4, updated_at = now()
		WHERE workspace_id = $1 AND id = $2
		RETURNING `+documentColumns,
		workspaceID, documentID, title, visibility,
	)
}

// MarkProcessing moves an upload to "processing" and queues its content check together.
func (r *Repository) MarkProcessing(ctx context.Context, workspaceID, documentID uuid.UUID, validate outbox.Job) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE documents SET status = 'processing', updated_at = now()
			WHERE workspace_id = $1 AND id = $2 AND status = 'uploading'`,
			workspaceID, documentID)
		if err != nil {
			return fmt.Errorf("update status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrWrongStatus // completed twice, or deleted meanwhile
		}
		return outbox.Enqueue(ctx, tx, validate)
	})
}

func (r *Repository) MarkReady(ctx context.Context, documentID uuid.UUID, sha256 []byte) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE documents SET status = 'ready', sha256 = $2, updated_at = now()
		WHERE id = $1 AND status = 'processing'`,
		documentID, sha256)
	return err
}

// MarkFailed records why a file was rejected and queues deleting it, in one transaction.
func (r *Repository) MarkFailed(ctx context.Context, documentID uuid.UUID, reason string, cleanup outbox.Job) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE documents SET status = 'failed', failure_reason = $2, updated_at = now()
			WHERE id = $1 AND status = 'processing'`,
			documentID, reason)
		if err != nil {
			return fmt.Errorf("update status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // already decided by an earlier run
		}
		return outbox.Enqueue(ctx, tx, cleanup)
	})
}

// Delete removes the row, which blocks access at once, and queues deleting the stored file.
// Both happen in one transaction: there is never a deleted document whose file is forgotten.
func (r *Repository) Delete(ctx context.Context, workspaceID, documentID uuid.UUID, cleanup outbox.Job) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM documents WHERE workspace_id = $1 AND id = $2`, workspaceID, documentID)
		if err != nil {
			return fmt.Errorf("delete document: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return outbox.Enqueue(ctx, tx, cleanup)
	})
}

func (r *Repository) AbandonedUploads(ctx context.Context, createdBefore time.Time, limit int) ([]domain.Document, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+documentColumns+`
		FROM documents
		WHERE status = 'uploading' AND created_at < $1
		ORDER BY created_at
		LIMIT $2`,
		createdBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("select abandoned uploads: %w", err)
	}
	docs, err := pgx.CollectRows(rows, scanDocument)
	if err != nil {
		return nil, fmt.Errorf("scan abandoned uploads: %w", err)
	}
	return docs, nil
}

// DeleteAbandonedUpload only deletes a document that is still "uploading", so an upload that
// completes during the sweep is left alone.
func (r *Repository) DeleteAbandonedUpload(ctx context.Context, documentID uuid.UUID, cleanup outbox.Job) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM documents WHERE id = $1 AND status = 'uploading'`, documentID)
		if err != nil {
			return fmt.Errorf("delete abandoned upload: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return outbox.Enqueue(ctx, tx, cleanup)
	})
}
