// Package app implements asking questions about a workspace's documents.
//
// The Core API decides access; the RAG service only searches within what it is given:
//   - the caller must be a member of the workspace (else ErrNotFound, as everywhere);
//   - the conversation must be the caller's own (else ErrNotFound);
//   - the RAG service receives the visibilities the caller's role may read, and re-checks
//     every document it finds against Postgres;
//   - earlier answers that cite a document the caller can no longer read are hidden, and are
//     not sent to the model as history.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"enclave/api/internal/authn"
	"enclave/api/internal/chat/domain"
	documentdomain "enclave/api/internal/document/domain"
	workspacedomain "enclave/api/internal/workspace/domain"
)

// maxHistoryMessages bounds how much of the conversation goes back to the model.
const maxHistoryMessages = 10

// Memberships is the part of the workspace context chat depends on.
type Memberships interface {
	MemberRole(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) (workspacedomain.Role, error)
}

// Documents is the part of the document context chat depends on.
type Documents interface {
	Readable(ctx context.Context, workspaceID uuid.UUID, role workspacedomain.Role, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// AnswerRequest is what the RAG service needs to answer.
type AnswerRequest struct {
	WorkspaceID  uuid.UUID
	Visibilities []documentdomain.Visibility
	Question     string
	History      []Turn
	RequestID    string
}

type Turn struct {
	Role    domain.MessageRole
	Content string
}

// AnswerEventKind says which field of an AnswerEvent is set.
type AnswerEventKind int

const (
	EventSources AnswerEventKind = iota + 1 // first: the passages found, numbered
	EventDelta                              // a piece of the answer text
	EventDone                               // last: the source numbers the answer cites
)

// AnswerEvent is one event of a streamed answer.
type AnswerEvent struct {
	Kind    AnswerEventKind
	Sources []domain.Citation // EventSources; empty when nothing relevant was found
	Delta   string            // EventDelta
	Cited   []int             // EventDone
}

// Answerer is the RAG service. Answer calls onEvent for each event in order and returns
// when the answer is complete, the context ends, or onEvent returns an error.
type Answerer interface {
	Answer(ctx context.Context, req AnswerRequest, onEvent func(AnswerEvent) error) error
}

// Repository stores conversations. Conversation methods take the owner, so a query can never
// return another member's conversation.
type Repository interface {
	CreateConversation(ctx context.Context, workspaceID, userID uuid.UUID) (domain.Conversation, error)
	Conversations(ctx context.Context, workspaceID, userID uuid.UUID) ([]domain.Conversation, error)
	Conversation(ctx context.Context, workspaceID, userID, conversationID uuid.UUID) (domain.Conversation, error)
	DeleteConversation(ctx context.Context, workspaceID, userID, conversationID uuid.UUID) error
	Messages(ctx context.Context, conversationID uuid.UUID) ([]domain.Message, error)
	// AddMessage appends a message and touches the conversation; title is set only if the
	// conversation has none yet.
	AddMessage(ctx context.Context, conversationID uuid.UUID, message domain.Message, title string) (domain.Message, error)
}

// AskOutput receives the answer as it is produced.
type AskOutput interface {
	Sources(sources []domain.Citation) error
	Delta(text string) error
}

type Service struct {
	memberships Memberships
	documents   Documents
	answerer    Answerer
	repo        Repository
}

func NewService(memberships Memberships, documents Documents, answerer Answerer, repo Repository) *Service {
	return &Service{memberships: memberships, documents: documents, answerer: answerer, repo: repo}
}

// StartConversation creates an empty conversation. Every member may ask questions; what
// they can get answers from depends on their role.
func (s *Service) StartConversation(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) (domain.Conversation, error) {
	if _, err := s.requireMember(ctx, caller, workspaceID); err != nil {
		return domain.Conversation{}, err
	}
	conversation, err := s.repo.CreateConversation(ctx, workspaceID, caller.UserID)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("create conversation: %w", err)
	}
	return conversation, nil
}

func (s *Service) ListConversations(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) ([]domain.Conversation, error) {
	if _, err := s.requireMember(ctx, caller, workspaceID); err != nil {
		return nil, err
	}
	conversations, err := s.repo.Conversations(ctx, workspaceID, caller.UserID)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	return conversations, nil
}

// GetConversation returns the conversation with its messages; answers citing a document the
// caller can no longer read are marked Hidden.
func (s *Service) GetConversation(ctx context.Context, caller authn.Principal, workspaceID, conversationID uuid.UUID) (domain.Conversation, []domain.Message, error) {
	role, err := s.requireMember(ctx, caller, workspaceID)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	conversation, err := s.repo.Conversation(ctx, workspaceID, caller.UserID, conversationID)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	messages, err := s.visibleMessages(ctx, workspaceID, role, conversation.ID)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	return conversation, messages, nil
}

func (s *Service) DeleteConversation(ctx context.Context, caller authn.Principal, workspaceID, conversationID uuid.UUID) error {
	if _, err := s.requireMember(ctx, caller, workspaceID); err != nil {
		return err
	}
	return s.repo.DeleteConversation(ctx, workspaceID, caller.UserID, conversationID)
}

// Ask answers a question in a conversation, streaming the answer to out, and stores both the
// question and the finished answer. If the answer does not finish (the caller left, the RAG
// service failed), only the question is stored.
func (s *Service) Ask(ctx context.Context, caller authn.Principal, workspaceID, conversationID uuid.UUID, rawQuestion, requestID string, out AskOutput) (domain.Message, error) {
	role, err := s.requireMember(ctx, caller, workspaceID)
	if err != nil {
		return domain.Message{}, err
	}
	conversation, err := s.repo.Conversation(ctx, workspaceID, caller.UserID, conversationID)
	if err != nil {
		return domain.Message{}, err
	}
	question, err := domain.CleanQuestion(rawQuestion)
	if err != nil {
		return domain.Message{}, err
	}
	earlier, err := s.visibleMessages(ctx, workspaceID, role, conversation.ID)
	if err != nil {
		return domain.Message{}, err
	}

	title := domain.TitleFrom(question)
	if _, err := s.repo.AddMessage(ctx, conversation.ID, domain.Message{Role: domain.RoleUser, Content: question}, title); err != nil {
		return domain.Message{}, fmt.Errorf("store question: %w", err)
	}

	var (
		answer  strings.Builder
		sources []domain.Citation
		cited   []int
	)
	history, historyIDs := historyFrom(earlier)
	req := AnswerRequest{
		WorkspaceID:  workspaceID,
		Visibilities: documentdomain.SearchableVisibilities(role),
		Question:     question,
		History:      history,
		RequestID:    requestID,
	}
	err = s.answerer.Answer(ctx, req, func(event AnswerEvent) error {
		switch event.Kind {
		case EventSources:
			sources = event.Sources
			return out.Sources(sources)
		case EventDelta:
			answer.WriteString(event.Delta)
			return out.Delta(event.Delta)
		case EventDone:
			cited = event.Cited
		}
		return nil
	})
	if err != nil {
		return domain.Message{}, err
	}

	citations := domain.CiteSources(sources, cited)
	// Without retrieved passages RAG returns a fixed refusal before using history.
	dependencies := make([]uuid.UUID, 0)
	if len(sources) > 0 {
		ids := make(map[uuid.UUID]struct{})
		for _, id := range historyIDs {
			ids[id] = struct{}{}
		}
		for _, source := range sources {
			ids[source.DocumentID] = struct{}{}
		}
		for id := range ids {
			dependencies = append(dependencies, id)
		}
	}
	reply := domain.Message{
		SourceDocumentIDs: dependencies,
		Role:              domain.RoleAssistant,
		Content:           answer.String(),
		Citations:         citations,
		Grounded:          len(citations) > 0,
	}
	// The answer is complete: store it even if the caller hangs up right now.
	stored, err := s.repo.AddMessage(context.WithoutCancel(ctx), conversation.ID, reply, title)
	if err != nil {
		return domain.Message{}, fmt.Errorf("store answer: %w", err)
	}
	return stored, nil
}

// visibleMessages loads the conversation and hides answers whose sources the caller with
// role can no longer read. Access is checked now, not when the answer was given: a document
// unshared since then hides every answer that quoted it.
func (s *Service) visibleMessages(ctx context.Context, workspaceID uuid.UUID, role workspacedomain.Role, conversationID uuid.UUID) ([]domain.Message, error) {
	messages, err := s.repo.Messages(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	var ids []uuid.UUID
	for _, message := range messages {
		ids = append(ids, message.SourceDocumentIDs...)
	}
	readable, err := s.documents.Readable(ctx, workspaceID, role, ids)
	if err != nil {
		return nil, fmt.Errorf("check sources: %w", err)
	}
	return domain.HideUnreadable(messages, readable), nil
}

// historyFrom keeps the latest messages for the model, leaving out hidden answers.
func historyFrom(messages []domain.Message) ([]Turn, []uuid.UUID) {
	visible := make([]domain.Message, 0, maxHistoryMessages)
	for i := len(messages) - 1; i >= 0 && len(visible) < maxHistoryMessages; i-- {
		if !messages[i].Hidden {
			visible = append(visible, messages[i])
		}
	}
	var turns []Turn
	var ids []uuid.UUID
	for i := len(visible) - 1; i >= 0; i-- {
		m := visible[i]
		turns = append(turns, Turn{Role: m.Role, Content: m.Content})
		ids = append(ids, m.SourceDocumentIDs...)
	}
	return turns, ids
}

// requireMember returns the caller's role, or domain.ErrNotFound for non-members: as
// everywhere in the API, outsiders cannot tell whether a workspace exists.
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
