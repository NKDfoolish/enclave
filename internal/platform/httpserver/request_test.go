package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadJSON(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	tests := []struct {
		name        string
		contentType string
		body        string
		wantErr     bool
	}{
		{"valid object", "application/json", `{"name":"acme"}`, false},
		{"charset parameter is fine", "application/json; charset=utf-8", `{"name":"acme"}`, false},
		{"form content type", "application/x-www-form-urlencoded", `{"name":"acme"}`, true},
		{"missing content type", "", `{"name":"acme"}`, true},
		{"unknown field", "application/json", `{"nmae":"acme"}`, true},
		{"two objects", "application/json", `{"name":"a"}{"name":"b"}`, true},
		{"too large", "application/json", `{"name":"` + strings.Repeat("a", maxJSONBodyBytes) + `"}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			var got payload
			err := ReadJSON(httptest.NewRecorder(), req, &got)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
