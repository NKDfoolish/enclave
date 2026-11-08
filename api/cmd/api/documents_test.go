package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"enclave/api/internal/document/domain"
	"enclave/api/internal/platform/objectstore"
)

var samplePDF = []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\n%%EOF\n")

// createUpload asks the API for an upload URL and returns the response body.
func (u *testUser) createUpload(workspaceID, filename string, size int) (int, map[string]any) {
	u.t.Helper()
	return u.do(http.MethodPost, "/v1/workspaces/"+workspaceID+"/documents", map[string]any{
		"filename": filename, "size_bytes": size,
	})
}

// putToStorage sends content to a presigned upload URL the way the browser does: straight to
// object storage, without cookies, with exactly the headers the API returned.
func putToStorage(t *testing.T, upload map[string]any, content []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, upload["url"].(string), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range upload["headers"].(map[string]any) {
		req.Header.Set(name, value.(string))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// uploadDocument runs the whole upload flow and the background check; it returns the
// document as the API reports it afterwards.
func (u *testUser) uploadDocument(workspaceID, filename string, content []byte) map[string]any {
	u.t.Helper()
	status, body := u.createUpload(workspaceID, filename, len(content))
	wantStatus(u.t, "create upload "+filename, status, http.StatusCreated, body)
	documentID := body["document"].(map[string]any)["id"].(string)

	if status := putToStorage(u.t, body["upload"].(map[string]any), content); status != http.StatusOK {
		u.t.Fatalf("PUT to storage: status %d", status)
	}
	status, body = u.do(http.MethodPost, documentPath(workspaceID, documentID)+"/complete", nil)
	wantStatus(u.t, "complete upload", status, http.StatusAccepted, body)
	if body["status"] != "processing" {
		u.t.Fatalf("status after complete = %v, want processing", body["status"])
	}

	u.env.runJobs(u.t)
	status, body = u.do(http.MethodGet, documentPath(workspaceID, documentID), nil)
	wantStatus(u.t, "get document", status, http.StatusOK, body)
	return body
}

func documentPath(workspaceID, documentID string) string {
	return "/v1/workspaces/" + workspaceID + "/documents/" + documentID
}

// listIDs returns the IDs of every document the user can list in the workspace.
func (u *testUser) listIDs(workspaceID string) []string {
	u.t.Helper()
	status, body := u.do(http.MethodGet, "/v1/workspaces/"+workspaceID+"/documents", nil)
	wantStatus(u.t, "list documents", status, http.StatusOK, body)
	var ids []string
	for _, d := range body["documents"].([]any) {
		ids = append(ids, d.(map[string]any)["id"].(string))
	}
	return ids
}

// downloadLocation requests a download and returns the status and redirect target.
func (u *testUser) downloadLocation(workspaceID, documentID string) (int, string) {
	u.t.Helper()
	req, err := http.NewRequestWithContext(u.t.Context(), http.MethodGet, u.env.server.URL+documentPath(workspaceID, documentID)+"/download", nil)
	if err != nil {
		u.t.Fatal(err)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func (env *testEnv) objectExists(t *testing.T, workspaceID, documentID string) bool {
	t.Helper()
	_, err := env.store.Size(context.Background(), domain.StorageKey(uuid.MustParse(workspaceID), uuid.MustParse(documentID)))
	if errors.Is(err, objectstore.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func TestDocuments(t *testing.T) {
	env := newTestEnv(t)
	admin := env.newUser(t, "Admin")
	staff := env.newUser(t, "Staff")
	client := env.newUser(t, "Client")
	outsider := env.newUser(t, "Outsider")

	workspaceID := admin.createWorkspace("Acme")
	admin.addMember(workspaceID, staff, "staff")
	admin.addMember(workspaceID, client, "client")

	policy := staff.uploadDocument(workspaceID, "Support policy.pdf", samplePDF)
	policyID := policy["id"].(string)
	policyPath := documentPath(workspaceID, policyID)

	t.Run("staff upload is checked, ready and internal by default", func(t *testing.T) {
		if policy["status"] != "ready" || policy["visibility"] != "internal" || policy["title"] != "Support policy" {
			t.Fatalf("document = %v", policy)
		}
		if !env.objectExists(t, workspaceID, policyID) {
			t.Fatal("file must be in storage")
		}
	})

	t.Run("clients cannot see internal documents", func(t *testing.T) {
		if contains(client.listIDs(workspaceID), policyID) {
			t.Fatal("client lists an internal document")
		}
		status, body := client.do(http.MethodGet, policyPath, nil)
		wantStatus(t, "get internal as client", status, http.StatusNotFound, body)
		status, _ = client.downloadLocation(workspaceID, policyID)
		if status != http.StatusNotFound {
			t.Fatalf("download internal as client: status %d, want 404", status)
		}
	})

	t.Run("sharing lets clients see and download it", func(t *testing.T) {
		status, body := staff.do(http.MethodPatch, policyPath, map[string]string{"visibility": "client_common"})
		wantStatus(t, "share", status, http.StatusOK, body)
		if !contains(client.listIDs(workspaceID), policyID) {
			t.Fatal("client does not list the shared document")
		}

		status, location := client.downloadLocation(workspaceID, policyID)
		if status != http.StatusFound || !strings.Contains(location, "X-Amz-Signature=") {
			t.Fatalf("download: status %d, location %q; want a 302 to a presigned URL", status, location)
		}
		// The redirect target is fetched without cookies: the signature alone grants access.
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, location, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		content, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(content, samplePDF) {
			t.Fatalf("presigned GET: status %d, %d bytes", resp.StatusCode, len(content))
		}
		if disposition := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") {
			t.Fatalf("Content-Disposition = %q, want attachment", disposition)
		}
	})

	t.Run("unsharing blocks clients on the next request", func(t *testing.T) {
		status, body := staff.do(http.MethodPatch, policyPath, map[string]string{"visibility": "internal"})
		wantStatus(t, "unshare", status, http.StatusOK, body)
		status, body = client.do(http.MethodGet, policyPath, nil)
		wantStatus(t, "get after unshare", status, http.StatusNotFound, body)
		status, _ = client.downloadLocation(workspaceID, policyID)
		if status != http.StatusNotFound {
			t.Fatalf("download after unshare: status %d, want 404", status)
		}
	})

	t.Run("clients cannot upload, change or delete documents", func(t *testing.T) {
		status, body := client.createUpload(workspaceID, "x.pdf", 10)
		wantStatus(t, "client upload", status, http.StatusForbidden, body)
		status, body = client.do(http.MethodPatch, policyPath, map[string]string{"title": "hacked"})
		wantStatus(t, "client rename", status, http.StatusForbidden, body)
		status, body = client.do(http.MethodDelete, policyPath, nil)
		wantStatus(t, "client delete", status, http.StatusForbidden, body)

		// The role is checked before the document is looked up, so a made-up ID gets the same
		// 403: the answer reveals nothing about which documents exist.
		status, body = client.do(http.MethodDelete, documentPath(workspaceID, uuid.NewString()), nil)
		wantStatus(t, "client delete of unknown ID", status, http.StatusForbidden, body)
	})

	t.Run("outsiders and other workspaces get 404", func(t *testing.T) {
		status, body := outsider.do(http.MethodGet, "/v1/workspaces/"+workspaceID+"/documents", nil)
		wantStatus(t, "outsider list", status, http.StatusNotFound, body)
		status, body = outsider.do(http.MethodGet, policyPath, nil)
		wantStatus(t, "outsider get", status, http.StatusNotFound, body)

		// A real document ID under the outsider's own workspace path: IDs never cross tenants.
		otherWorkspace := outsider.createWorkspace("Other")
		status, body = outsider.do(http.MethodGet, documentPath(otherWorkspace, policyID), nil)
		wantStatus(t, "foreign document in own workspace", status, http.StatusNotFound, body)
		status, body = outsider.do(http.MethodDelete, documentPath(otherWorkspace, policyID), nil)
		wantStatus(t, "delete foreign document", status, http.StatusNotFound, body)
	})

	t.Run("a file that is not what its name says is rejected and removed", func(t *testing.T) {
		fake := staff.uploadDocument(workspaceID, "invoice.pdf", []byte("MZ\x90\x00 this is an executable"))
		fakeID := fake["id"].(string)
		if fake["status"] != "failed" || !strings.Contains(fake["failure_reason"].(string), "not a PDF") {
			t.Fatalf("document = %v", fake)
		}
		status, _ := staff.downloadLocation(workspaceID, fakeID)
		if status != http.StatusConflict {
			t.Fatalf("download of a failed document: status %d, want 409", status)
		}
		if env.objectExists(t, workspaceID, fakeID) {
			t.Fatal("rejected file must be deleted from storage")
		}
	})

	t.Run("unsupported and oversized files are refused before upload", func(t *testing.T) {
		status, body := staff.createUpload(workspaceID, "setup.exe", 100)
		wantStatus(t, "exe", status, http.StatusUnsupportedMediaType, body)
		status, body = staff.createUpload(workspaceID, "page.html", 100)
		wantStatus(t, "html", status, http.StatusUnsupportedMediaType, body)
		status, body = staff.createUpload(workspaceID, "huge.pdf", testMaxDocumentBytes+1)
		wantStatus(t, "too large", status, http.StatusRequestEntityTooLarge, body)
	})

	t.Run("the presigned URL only accepts the declared size", func(t *testing.T) {
		status, body := staff.createUpload(workspaceID, "notes.md", 10)
		wantStatus(t, "create upload", status, http.StatusCreated, body)
		if status := putToStorage(t, body["upload"].(map[string]any), []byte("# a much longer file than declared")); status != http.StatusForbidden {
			t.Fatalf("PUT with a different size: status %d, want 403", status)
		}
	})

	t.Run("completing before uploading fails", func(t *testing.T) {
		status, body := staff.createUpload(workspaceID, "draft.md", 10)
		wantStatus(t, "create upload", status, http.StatusCreated, body)
		documentID := body["document"].(map[string]any)["id"].(string)
		status, body = staff.do(http.MethodPost, documentPath(workspaceID, documentID)+"/complete", nil)
		wantStatus(t, "complete without file", status, http.StatusConflict, body)
	})

	t.Run("deleting blocks access and removes the stored file", func(t *testing.T) {
		doomed := staff.uploadDocument(workspaceID, "old.md", []byte("# Old policy\n"))
		doomedID := doomed["id"].(string)

		status, body := staff.do(http.MethodDelete, documentPath(workspaceID, doomedID), nil)
		wantStatus(t, "delete", status, http.StatusNoContent, body)
		status, body = staff.do(http.MethodGet, documentPath(workspaceID, doomedID), nil)
		wantStatus(t, "get after delete", status, http.StatusNotFound, body)

		env.runJobs(t)
		if env.objectExists(t, workspaceID, doomedID) {
			t.Fatal("deleted document's file must be removed from storage")
		}
	})

	t.Run("abandoned uploads are cleaned up", func(t *testing.T) {
		status, body := staff.createUpload(workspaceID, "never-finished.md", 4)
		wantStatus(t, "create upload", status, http.StatusCreated, body)
		documentID := body["document"].(map[string]any)["id"].(string)
		putToStorage(t, body["upload"].(map[string]any), []byte("# hi"))
		if _, err := env.pool.Exec(context.Background(),
			`UPDATE documents SET created_at = now() - interval '2 hours' WHERE id = $1`, documentID); err != nil {
			t.Fatal(err)
		}

		if _, err := env.documents.CleanupAbandonedUploads(context.Background()); err != nil {
			t.Fatal(err)
		}
		env.runJobs(t)
		status, body = staff.do(http.MethodGet, documentPath(workspaceID, documentID), nil)
		wantStatus(t, "get abandoned upload", status, http.StatusNotFound, body)
		if env.objectExists(t, workspaceID, documentID) {
			t.Fatal("file of an abandoned upload must be removed")
		}
	})
}

func TestDocumentPagination(t *testing.T) {
	env := newTestEnv(t)
	staff := env.newUser(t, "Staff")
	workspaceID := staff.createWorkspace("Paging") // creator is admin, which can manage documents

	var created []string
	for i := range 5 {
		_, body := staff.createUpload(workspaceID, fmt.Sprintf("doc-%d.md", i), 10)
		created = append(created, body["document"].(map[string]any)["id"].(string))
	}

	status, page1 := staff.do(http.MethodGet, "/v1/workspaces/"+workspaceID+"/documents?limit=2", nil)
	wantStatus(t, "page 1", status, http.StatusOK, page1)
	cursor := page1["next_cursor"].(string)
	if cursor == "" || len(page1["documents"].([]any)) != 2 {
		t.Fatalf("page 1 = %v", page1)
	}

	// A document arriving between pages must not shift the next page.
	_, late := staff.createUpload(workspaceID, "late.md", 10)
	lateID := late["document"].(map[string]any)["id"].(string)

	seen := map[string]bool{}
	for _, d := range page1["documents"].([]any) {
		seen[d.(map[string]any)["id"].(string)] = true
	}
	for cursor != "" {
		status, page := staff.do(http.MethodGet, "/v1/workspaces/"+workspaceID+"/documents?limit=2&cursor="+cursor, nil)
		wantStatus(t, "next page", status, http.StatusOK, page)
		for _, d := range page["documents"].([]any) {
			id := d.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("document %s appeared twice", id)
			}
			seen[id] = true
		}
		cursor = page["next_cursor"].(string)
	}

	for _, id := range created {
		if !seen[id] {
			t.Fatalf("document %s was skipped", id)
		}
	}
	if seen[lateID] {
		t.Fatal("a document newer than the first page must not appear on later pages")
	}

	status, body := staff.do(http.MethodGet, "/v1/workspaces/"+workspaceID+"/documents?cursor=not-a-cursor", nil)
	wantStatus(t, "bad cursor", status, http.StatusBadRequest, body)
}
