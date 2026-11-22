// Package domain holds the document library model: documents, who may see and manage them,
// and which files are accepted.
package domain

import (
	"errors"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	workspacedomain "enclave/api/internal/workspace/domain"
)

// Visibility says who inside the workspace can see a document. Neither value is public on
// the internet or visible to other workspaces.
type Visibility string

const (
	VisibilityInternal     Visibility = "internal"      // admins and staff
	VisibilityClientCommon Visibility = "client_common" // also every client of the workspace
)

func ParseVisibility(s string) (Visibility, bool) {
	switch v := Visibility(s); v {
	case VisibilityInternal, VisibilityClientCommon:
		return v, true
	default:
		return "", false
	}
}

// Status tracks a document from upload to usable:
// uploading → processing → ready, or failed if the file is rejected.
type Status string

const (
	StatusUploading  Status = "uploading"  // upload URL issued, file not confirmed yet
	StatusProcessing Status = "processing" // file stored, content being checked
	StatusReady      Status = "ready"      // checked; can be downloaded
	StatusFailed     Status = "failed"     // rejected; the stored file is deleted
)

type Document struct {
	ID            uuid.UUID
	WorkspaceID   uuid.UUID
	Title         string
	Filename      string
	ContentType   ContentType
	SizeBytes     int64
	SHA256        []byte
	StorageKey    string
	Visibility    Visibility
	Status        Status
	FailureReason string
	UploadedBy    uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CanManage covers uploading, renaming, sharing and deleting documents.
func CanManage(role workspacedomain.Role) bool {
	return role == workspacedomain.RoleAdmin || role == workspacedomain.RoleStaff
}

// IsVisibleTo reports whether a member with role may see the document at all. Clients only
// see ready documents that were explicitly shared with them.
func (d Document) IsVisibleTo(role workspacedomain.Role) bool {
	if CanManage(role) {
		return true
	}
	return d.Visibility == VisibilityClientCommon && d.Status == StatusReady
}

// IsDownloadable: only files whose content has been checked can leave the store.
func (d Document) IsDownloadable() bool {
	return d.Status == StatusReady
}

// StorageKey is where the file lives in the bucket. It is built from IDs only, never from the
// user's filename, so a name like "../../x" cannot pick the location.
func StorageKey(workspaceID, documentID uuid.UUID) string {
	return "workspaces/" + workspaceID.String() + "/documents/" + documentID.String()
}

const maxFilenameLength = 255

// CleanFilename keeps only the last path element and rejects names with control characters,
// so the name is safe to show and to put in a download header.
func CleanFilename(name string) (string, bool) {
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, `\`, "/")))
	if name == "" || name == "." || name == "/" || utf8.RuneCountInString(name) > maxFilenameLength {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}

// DefaultTitle is the filename without its extension.
func DefaultTitle(filename string) string {
	return strings.TrimSuffix(filename, path.Ext(filename))
}

var (
	ErrInvalidInput = errors.New("invalid input")
	// ErrNotFound is also returned for documents the caller may not see, so their existence
	// is never revealed.
	ErrNotFound        = errors.New("not found")
	ErrForbidden       = errors.New("forbidden")
	ErrUnsupportedType = errors.New("unsupported file type")
	ErrTooLarge        = errors.New("file is too large")
	ErrNotReady        = errors.New("document is not ready")
	// ErrUploadIncomplete: "complete" was called but the stored file is missing or has a
	// different size than declared.
	ErrUploadIncomplete = errors.New("upload is missing or incomplete")
	// ErrWrongStatus: the action does not fit the document's current status.
	ErrWrongStatus = errors.New("document is in the wrong status for this action")
)
