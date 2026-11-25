package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

const maxJSONBodyBytes = 1 << 20 // 1 MiB

// ReadJSON decodes a single JSON object from the request body into dst.
//
// It requires Content-Type: application/json. Browsers cannot send that header in a
// cross-site form post, which closes off the simplest CSRF attacks on cookie-authenticated
// endpoints. It also caps the body size and rejects unknown fields, so typos in field names
// fail loudly instead of being ignored.
func ReadJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("content type must be application/json")
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must contain a single JSON object")
	}
	return nil
}
