// Package domain holds conversations: questions a member asks about the workspace's
// documents, and the answers with the sources they cite.
//
// A conversation belongs to the member who started it and nobody else, not even workspace
// admins. An answer keeps the documents it cited, so it can be hidden later if the member
// loses access to one of them.
package domain

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	MaxQuestionLength = 4000
	titleLength       = 60
)

type Conversation struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	UserID      uuid.UUID
	// Title is taken from the first question; empty until one is asked.
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
)

// Citation is a source an answer cites: [Number] in the answer text.
type Citation struct {
	Number     int
	DocumentID uuid.UUID
	Title      string
	Page       int    // 1-based PDF page, 0 when the format has no pages
	Section    string // heading path, empty when none
	Snippet    string
}

type Message struct {
	ID                int64
	Role              MessageRole
	Content           string
	Citations         []Citation
	SourceDocumentIDs []uuid.UUID
	// Grounded is true when the answer cites at least one source.
	Grounded  bool
	CreatedAt time.Time
	// Hidden is set when the reader can no longer read a document the answer cites. The
	// answer may quote that document, so its text must not be shown or sent to the model.
	Hidden bool
}

// HideUnreadable marks every answer that cites a document not in readable.
func HideUnreadable(messages []Message, readable map[uuid.UUID]bool) []Message {
	result := make([]Message, len(messages))
	for i, message := range messages {
		for _, id := range message.SourceDocumentIDs {
			if !readable[id] {
				message.Hidden = true
				break
			}
		}
		result[i] = message
	}
	return result
}

// CleanQuestion trims the question and checks its length.
func CleanQuestion(raw string) (string, error) {
	question := strings.TrimSpace(raw)
	if question == "" || utf8.RuneCountInString(question) > MaxQuestionLength {
		return "", ErrInvalidQuestion
	}
	return question, nil
}

// TitleFrom makes a conversation title from its first question.
func TitleFrom(question string) string {
	question = strings.Join(strings.Fields(question), " ")
	if utf8.RuneCountInString(question) <= titleLength {
		return question
	}
	runes := []rune(question)
	return strings.TrimSpace(string(runes[:titleLength])) + "…"
}

// CiteSources returns the sources the answer cited, by number, in citation order. Unknown
// numbers are ignored.
func CiteSources(sources []Citation, cited []int) []Citation {
	byNumber := map[int]Citation{}
	for _, source := range sources {
		byNumber[source.Number] = source
	}
	var citations []Citation
	for _, number := range cited {
		if source, ok := byNumber[number]; ok {
			citations = append(citations, source)
		}
	}
	return citations
}

var (
	// ErrNotFound is also returned for another member's conversation, so conversations of
	// others cannot be probed.
	ErrNotFound        = errors.New("conversation not found")
	ErrInvalidQuestion = errors.New("question must be 1-4000 characters")
	// ErrAnswerUnavailable: the RAG service, the gateway or every model is unavailable.
	ErrAnswerUnavailable = errors.New("answering is unavailable right now")
	// ErrNoModelAllowed: the workspace's AI settings allow no provider for the configured
	// model (e.g. only external models are configured and none is allowed).
	ErrNoModelAllowed = errors.New("the workspace's AI settings allow no model for answering")
)
