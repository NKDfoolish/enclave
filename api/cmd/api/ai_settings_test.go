package main

import (
	"net/http"
	"testing"
)

func TestAISettings(t *testing.T) {
	env := newTestEnv(t)
	admin := env.newUser(t, "admin")
	staff := env.newUser(t, "staff")
	client := env.newUser(t, "client")
	outsider := env.newUser(t, "outsider")
	workspaceID := admin.createWorkspace("AI settings")
	admin.addMember(workspaceID, staff, "staff")
	admin.addMember(workspaceID, client, "client")
	path := "/v1/workspaces/" + workspaceID + "/ai-settings"

	t.Run("new workspace allows no external provider", func(t *testing.T) {
		status, body := admin.do(http.MethodGet, path, nil)
		wantStatus(t, "get settings", status, http.StatusOK, body)
		if got := body["external_providers"].([]any); len(got) != 0 {
			t.Fatalf("external_providers = %v, want none by default", got)
		}
		if body["updated_at"] != nil {
			t.Fatalf("updated_at = %v, want null for the default", body["updated_at"])
		}
		if got := body["available_external_providers"].([]any); len(got) != 3 {
			t.Fatalf("available_external_providers = %v", got)
		}
	})

	t.Run("admin allows providers; list is sorted without duplicates", func(t *testing.T) {
		status, body := admin.do(http.MethodPut, path, map[string]any{
			"external_providers": []string{"nvidia", "anthropic", "nvidia"},
		})
		wantStatus(t, "update settings", status, http.StatusOK, body)
		got := body["external_providers"].([]any)
		if len(got) != 2 || got[0] != "anthropic" || got[1] != "nvidia" {
			t.Fatalf("external_providers = %v, want [anthropic nvidia]", got)
		}
		if body["updated_at"] == nil {
			t.Fatal("updated_at must be set once an admin saved the settings")
		}
	})

	t.Run("staff can read but not change; clients and outsiders cannot read", func(t *testing.T) {
		status, body := staff.do(http.MethodGet, path, nil)
		wantStatus(t, "staff reads", status, http.StatusOK, body)
		status, body = staff.do(http.MethodPut, path, map[string]any{"external_providers": []string{}})
		wantStatus(t, "staff updates", status, http.StatusForbidden, body)
		status, body = client.do(http.MethodGet, path, nil)
		wantStatus(t, "client reads", status, http.StatusForbidden, body)
		status, body = outsider.do(http.MethodGet, path, nil)
		wantStatus(t, "outsider reads", status, http.StatusNotFound, body)
	})

	t.Run("unknown provider is rejected", func(t *testing.T) {
		status, body := admin.do(http.MethodPut, path, map[string]any{"external_providers": []string{"ollama"}})
		wantStatus(t, "unknown provider", status, http.StatusBadRequest, body)
	})

	t.Run("empty list goes back to local providers only", func(t *testing.T) {
		status, body := admin.do(http.MethodPut, path, map[string]any{"external_providers": []string{}})
		wantStatus(t, "clear settings", status, http.StatusOK, body)
		if got := body["external_providers"].([]any); len(got) != 0 {
			t.Fatalf("external_providers = %v, want none", got)
		}
	})
}
