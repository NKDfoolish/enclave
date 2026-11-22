// Package app implements the document library use cases. Every use case first asks the
// workspace context for the caller's role: non-members get ErrNotFound, and a document the
// caller may not see is reported as ErrNotFound too, so its existence is never revealed.
//
// Files never pass through the API. Uploads go straight from the browser to object storage
// with a presigned PUT, a background job then checks the content, and downloads are
// redirects to short-lived presigned GETs (ADR 0003).
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"enclave/api/internal/document/domain"
	"enclave/api/internal/platform/authn"
	"enclave/api/internal/platform/objectstore"
	"enclave/api/internal/platform/outbox"
	workspacedomain "enclave/api/internal/workspace/domain"
)

const (
	uploadURLTTL   = 15 * time.Minute
	downloadURLTTL = 60 * time.Second
	maxTitleLength = 200
)

// Memberships is the part of the workspace context documents depend on.
type Memberships interface {
	MemberRole(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) (workspacedomain.Role, error)
}

// ObjectStore holds the original files.
type ObjectStore interface {
	PresignUpload(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (objectstore.PresignedUpload, error)
	PresignDownload(ctx context.Context, key, filename, contentType string, ttl time.Duration) (string, error)
	Size(ctx context.Context, key string) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// Repository stores document metadata. Methods that take an outbox.Job write it in the same
// transaction as the change, so follow-up work is never lost.
type Repository interface {
	Create(ctx context.Context, doc domain.Document) (domain.Document, error)
	Get(ctx context.Context, workspaceID, documentID uuid.UUID) (domain.Document, error)
	GetByID(ctx context.Context, documentID uuid.UUID) (domain.Document, error)
	List(ctx context.Context, query ListQuery) ([]domain.Document, error)
	UpdateDetails(ctx context.Context, workspaceID, documentID uuid.UUID, title string, visibility domain.Visibility) (domain.Document, error)
	MarkProcessing(ctx context.Context, workspaceID, documentID uuid.UUID, validate outbox.Job) error
	MarkReady(ctx context.Context, documentID uuid.UUID, sha256 []byte) error
	MarkFailed(ctx context.Context, documentID uuid.UUID, reason string, cleanup outbox.Job) error
	Delete(ctx context.Context, workspaceID, documentID uuid.UUID, cleanup outbox.Job) error
	AbandonedUploads(ctx context.Context, createdBefore time.Time, limit int) ([]domain.Document, error)
	DeleteAbandonedUpload(ctx context.Context, documentID uuid.UUID, cleanup outbox.Job) error
}

// ListQuery selects one page of a workspace's documents, newest first.
type ListQuery struct {
	WorkspaceID uuid.UUID
	// SharedReadyOnly limits the page to what clients may see.
	SharedReadyOnly bool
	After           *Cursor
	Limit           int
}

type Service struct {
	memberships Memberships
	repo        Repository
	store       ObjectStore
	maxBytes    int64
	now         func() time.Time
}

func NewService(memberships Memberships, repo Repository, store ObjectStore, maxBytes int64) *Service {
	return &Service{memberships: memberships, repo: repo, store: store, maxBytes: maxBytes, now: time.Now}
}

type CreateUploadInput struct {
	Title     string
	Filename  string
	SizeBytes int64
}

// UploadTicket is a new document in status "uploading" plus the presigned PUT for its file.
type UploadTicket struct {
	Document domain.Document
	Upload   objectstore.PresignedUpload
}

// CreateUpload registers a document and returns a URL the browser uses to upload the file
// directly to storage. The declared size and type are signed into the URL.
func (s *Service) CreateUpload(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID, in CreateUploadInput) (UploadTicket, error) {
	if _, err := s.requireManager(ctx, caller, workspaceID); err != nil {
		return UploadTicket{}, err
	}

	filename, ok := domain.CleanFilename(in.Filename)
	if !ok {
		return UploadTicket{}, invalidInput("filename is empty, too long or contains control characters")
	}
	contentType, ok := domain.ContentTypeFromFilename(filename)
	if !ok {
		return UploadTicket{}, domain.ErrUnsupportedType
	}
	if in.SizeBytes <= 0 {
		return UploadTicket{}, invalidInput("size_bytes must be positive")
	}
	if in.SizeBytes > s.maxBytes {
		return UploadTicket{}, domain.ErrTooLarge
	}
	title, err := cleanTitle(in.Title, filename)
	if err != nil {
		return UploadTicket{}, err
	}

	documentID := uuid.New()
	doc, err := s.repo.Create(ctx, domain.Document{
		ID:          documentID,
		WorkspaceID: workspaceID,
		Title:       title,
		Filename:    filename,
		ContentType: contentType,
		SizeBytes:   in.SizeBytes,
		StorageKey:  domain.StorageKey(workspaceID, documentID),
		Visibility:  domain.VisibilityInternal,
		Status:      domain.StatusUploading,
		UploadedBy:  caller.UserID,
	})
	if err != nil {
		return UploadTicket{}, fmt.Errorf("create document: %w", err)
	}

	upload, err := s.store.PresignUpload(ctx, doc.StorageKey, string(doc.ContentType), doc.SizeBytes, uploadURLTTL)
	if err != nil {
		return UploadTicket{}, err
	}
	return UploadTicket{Document: doc, Upload: upload}, nil
}

// CompleteUpload is called after the browser's PUT succeeded. It checks the file arrived with
// the declared size, then queues the content check. The document is "processing" until then.
func (s *Service) CompleteUpload(ctx context.Context, caller authn.Principal, workspaceID, documentID uuid.UUID) (domain.Document, error) {
	if _, err := s.requireManager(ctx, caller, workspaceID); err != nil {
		return domain.Document{}, err
	}
	doc, err := s.repo.Get(ctx, workspaceID, documentID)
	if err != nil {
		return domain.Document{}, err
	}
	if doc.Status != domain.StatusUploading {
		return domain.Document{}, domain.ErrWrongStatus
	}

	size, err := s.store.Size(ctx, doc.StorageKey)
	if errors.Is(err, objectstore.ErrNotFound) {
		return domain.Document{}, domain.ErrUploadIncomplete
	}
	if err != nil {
		return domain.Document{}, fmt.Errorf("check uploaded file: %w", err)
	}
	if size != doc.SizeBytes {
		return domain.Document{}, domain.ErrUploadIncomplete
	}

	job, err := outbox.NewJob(JobValidateUpload, validateUploadPayload{DocumentID: doc.ID})
	if err != nil {
		return domain.Document{}, err
	}
	if err := s.repo.MarkProcessing(ctx, workspaceID, documentID, job); err != nil {
		return domain.Document{}, fmt.Errorf("mark document %s processing: %w", documentID, err)
	}
	doc.Status = domain.StatusProcessing
	return doc, nil
}

// Page is one page of documents. NextCursor is empty on the last page.
type Page struct {
	Documents  []domain.Document
	NextCursor string
}

// List returns documents newest first, using keyset pagination: the cursor is the position of
// the last row seen, so new uploads between requests never shift or repeat rows.
func (s *Service) List(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID, cursor string, limit int) (Page, error) {
	role, err := s.requireMember(ctx, caller, workspaceID)
	if err != nil {
		return Page{}, err
	}
	after, err := DecodeCursor(cursor)
	if err != nil {
		return Page{}, invalidInput("cursor is not valid")
	}
	limit = clampPageSize(limit)

	// Ask for one extra row to learn whether another page exists.
	docs, err := s.repo.List(ctx, ListQuery{
		WorkspaceID:     workspaceID,
		SharedReadyOnly: !domain.CanManage(role),
		After:           after,
		Limit:           limit + 1,
	})
	if err != nil {
		return Page{}, fmt.Errorf("list documents of workspace %s: %w", workspaceID, err)
	}

	page := Page{Documents: docs}
	if len(docs) > limit {
		page.Documents = docs[:limit]
		last := page.Documents[limit-1]
		page.NextCursor = EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return page, nil
}

func (s *Service) Get(ctx context.Context, caller authn.Principal, workspaceID, documentID uuid.UUID) (domain.Document, error) {
	role, err := s.requireMember(ctx, caller, workspaceID)
	if err != nil {
		return domain.Document{}, err
	}
	doc, err := s.repo.Get(ctx, workspaceID, documentID)
	if err != nil {
		return domain.Document{}, err
	}
	if !doc.IsVisibleTo(role) {
		return domain.Document{}, domain.ErrNotFound
	}
	return doc, nil
}

// DownloadURL checks access now and returns a presigned GET that works for 60 seconds.
func (s *Service) DownloadURL(ctx context.Context, caller authn.Principal, workspaceID, documentID uuid.UUID) (string, error) {
	doc, err := s.Get(ctx, caller, workspaceID, documentID)
	if err != nil {
		return "", err
	}
	if !doc.IsDownloadable() {
		return "", domain.ErrNotReady
	}
	return s.store.PresignDownload(ctx, doc.StorageKey, doc.Filename, string(doc.ContentType), downloadURLTTL)
}

// UpdateInput holds the fields to change; nil means "leave as is".
type UpdateInput struct {
	Title      *string
	Visibility *string
}

// Update renames a document or shares/unshares it. Unsharing takes effect on the next
// request because visibility is read from the database every time.
func (s *Service) Update(ctx context.Context, caller authn.Principal, workspaceID, documentID uuid.UUID, in UpdateInput) (domain.Document, error) {
	if _, err := s.requireManager(ctx, caller, workspaceID); err != nil {
		return domain.Document{}, err
	}
	doc, err := s.repo.Get(ctx, workspaceID, documentID)
	if err != nil {
		return domain.Document{}, err
	}

	title, visibility := doc.Title, doc.Visibility
	if in.Title != nil {
		if title, err = cleanTitle(*in.Title, ""); err != nil {
			return domain.Document{}, err
		}
	}
	if in.Visibility != nil {
		var ok bool
		if visibility, ok = domain.ParseVisibility(*in.Visibility); !ok {
			return domain.Document{}, invalidInput("visibility must be internal or client_common")
		}
	}

	updated, err := s.repo.UpdateDetails(ctx, workspaceID, documentID, title, visibility)
	if err != nil {
		return domain.Document{}, fmt.Errorf("update document %s: %w", documentID, err)
	}
	return updated, nil
}

// Delete removes the document first, which blocks access immediately, and queues deleting the
// stored file in the same transaction.
func (s *Service) Delete(ctx context.Context, caller authn.Principal, workspaceID, documentID uuid.UUID) error {
	if _, err := s.requireManager(ctx, caller, workspaceID); err != nil {
		return err
	}
	doc, err := s.repo.Get(ctx, workspaceID, documentID)
	if err != nil {
		return err
	}
	job, err := deleteObjectJob(doc.StorageKey)
	if err != nil {
		return err
	}
	if err := s.repo.Delete(ctx, workspaceID, documentID, job); err != nil {
		return fmt.Errorf("delete document %s: %w", documentID, err)
	}
	return nil
}

// requireMember returns the caller's role, or ErrNotFound if they are not a member.
func (s *Service) requireMember(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) (workspacedomain.Role, error) {
	role, err := s.memberships.MemberRole(ctx, caller, workspaceID)
	if errors.Is(err, workspacedomain.ErrNotFound) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load role in workspace %s: %w", workspaceID, err)
	}
	return role, nil
}

func (s *Service) requireManager(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) (workspacedomain.Role, error) {
	role, err := s.requireMember(ctx, caller, workspaceID)
	if err != nil {
		return "", err
	}
	if !domain.CanManage(role) {
		return "", domain.ErrForbidden
	}
	return role, nil
}

// cleanTitle trims title; an empty title falls back to the filename without its extension.
func cleanTitle(title, filename string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = domain.DefaultTitle(filename)
	}
	if title == "" || utf8.RuneCountInString(title) > maxTitleLength {
		return "", invalidInput(fmt.Sprintf("title must be 1-%d characters", maxTitleLength))
	}
	return title, nil
}

func invalidInput(message string) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidInput, message)
}
