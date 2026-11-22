package app

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// Cursor marks the last document of a page. The next page starts strictly after it in
// (created_at DESC, id DESC) order; the id breaks ties between equal timestamps.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// EncodeCursor makes an opaque token for clients. Postgres stores microseconds, so the
// timestamp is encoded at that precision to compare exactly with the stored value.
func EncodeCursor(c Cursor) string {
	raw := strconv.FormatInt(c.CreatedAt.UnixMicro(), 10) + "_" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token from EncodeCursor. An empty token means the first page.
func DecodeCursor(token string) (*Cursor, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	micros, id, ok := strings.Cut(string(raw), "_")
	if !ok {
		return nil, errors.New("malformed cursor")
	}
	createdAt, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return nil, err
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	return &Cursor{CreatedAt: time.UnixMicro(createdAt), ID: parsedID}, nil
}

func clampPageSize(limit int) int {
	if limit <= 0 {
		return defaultPageSize
	}
	return min(limit, maxPageSize)
}
