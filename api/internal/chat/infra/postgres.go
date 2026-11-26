// Package infra stores conversations in Postgres and asks the RAG service for answers. Every
// conversation query filters by workspace and owner, so one member's conversations can never
// be read through another's request.
package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enclave/api/internal/chat/domain"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

const conversationColumns = `id, workspace_id, user_id, title, created_at, updated_at`

func scanConversation(row pgx.CollectableRow) (domain.Conversation, error) {
	var c domain.Conversation
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.UserID, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (r *Repository) CreateConversation(ctx context.Context, workspaceID, userID uuid.UUID) (domain.Conversation, error) {
	rows, err := r.pool.Query(ctx,
		`INSERT INTO conversations (workspace_id, user_id) VALUES ($1, $2) RETURNING `+conversationColumns,
		workspaceID, userID)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("insert conversation: %w", err)
	}
	return pgx.CollectExactlyOneRow(rows, scanConversation)
}

func (r *Repository) Conversations(ctx context.Context, workspaceID, userID uuid.UUID) ([]domain.Conversation, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+conversationColumns+` FROM conversations
		WHERE workspace_id = $1 AND user_id = $2
		ORDER BY updated_at DESC, id
		LIMIT 100`,
		workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("select conversations: %w", err)
	}
	return pgx.CollectRows(rows, scanConversation)
}

func (r *Repository) Conversation(ctx context.Context, workspaceID, userID, conversationID uuid.UUID) (domain.Conversation, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+conversationColumns+` FROM conversations
		WHERE workspace_id = $1 AND user_id = $2 AND id = $3`,
		workspaceID, userID, conversationID)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("select conversation: %w", err)
	}
	conversation, err := pgx.CollectExactlyOneRow(rows, scanConversation)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Conversation{}, domain.ErrNotFound
	}
	return conversation, err
}

func (r *Repository) DeleteConversation(ctx context.Context, workspaceID, userID, conversationID uuid.UUID) error {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM conversations WHERE workspace_id = $1 AND user_id = $2 AND id = $3`,
		workspaceID, userID, conversationID)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// citationJSON is how a citation is stored in conversation_messages.citations.
type citationJSON struct {
	Number     int       `json:"number"`
	DocumentID uuid.UUID `json:"document_id"`
	Title      string    `json:"title"`
	Page       int       `json:"page,omitempty"`
	Section    string    `json:"section,omitempty"`
	Snippet    string    `json:"snippet,omitempty"`
}

func (r *Repository) Messages(ctx context.Context, conversationID uuid.UUID) ([]domain.Message, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, role, content, citations, grounded, created_at, source_document_ids
		FROM conversation_messages WHERE conversation_id = $1 ORDER BY id`,
		conversationID)
	if err != nil {
		return nil, fmt.Errorf("select messages: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Message, error) {
		var (
			message   domain.Message
			citations []citationJSON
		)
		if err := row.Scan(&message.ID, &message.Role, &message.Content, &citations, &message.Grounded, &message.CreatedAt, &message.SourceDocumentIDs); err != nil {
			return domain.Message{}, err
		}
		message.Hidden = message.Role == domain.RoleAssistant && message.SourceDocumentIDs == nil
		for _, c := range citations {
			message.Citations = append(message.Citations, domain.Citation(c))
		}
		return message, nil
	})
}

func (r *Repository) AddMessage(ctx context.Context, conversationID uuid.UUID, message domain.Message, title string) (domain.Message, error) {
	citations := make([]citationJSON, 0, len(message.Citations))
	for _, c := range message.Citations {
		citations = append(citations, citationJSON(c))
	}
	encoded, err := json.Marshal(citations)
	if err != nil {
		return domain.Message{}, fmt.Errorf("encode citations: %w", err)
	}

	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO conversation_messages (conversation_id, role, content, citations, grounded, source_document_ids)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, created_at`,
			conversationID, message.Role, message.Content, encoded, message.Grounded, message.SourceDocumentIDs,
		).Scan(&message.ID, &message.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		_, err = tx.Exec(ctx, `
			UPDATE conversations
			SET updated_at = now(), title = CASE WHEN title = '' THEN $2 ELSE title END
			WHERE id = $1`,
			conversationID, title)
		if err != nil {
			return fmt.Errorf("touch conversation: %w", err)
		}
		return nil
	})
	return message, err
}
