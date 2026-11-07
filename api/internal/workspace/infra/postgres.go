// Package infra stores workspaces, memberships and invitations in Postgres.
// Every query is scoped by workspace_id so one tenant can never read another's rows.
package infra

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enclave/api/internal/platform/postgres"
	"enclave/api/internal/workspace/domain"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) CreateWorkspace(ctx context.Context, name string, adminID uuid.UUID) (domain.Workspace, error) {
	var workspace domain.Workspace
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO workspaces (name) VALUES ($1) RETURNING id, name, created_at`, name,
		).Scan(&workspace.ID, &workspace.Name, &workspace.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert workspace: %w", err)
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			workspace.ID, adminID, domain.RoleAdmin,
		)
		if err != nil {
			return fmt.Errorf("insert admin membership: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Workspace{}, err
	}
	return workspace, nil
}

func (r *Repository) WorkspacesForUser(ctx context.Context, userID uuid.UUID) ([]domain.UserWorkspace, error) {
	const query = `
		SELECT w.id, w.name, w.created_at, m.role
		FROM memberships m
		JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = $1
		ORDER BY w.name, w.id`

	rows, err := r.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("select workspaces: %w", err)
	}
	workspaces, err := pgx.CollectRows(rows, scanUserWorkspace)
	if err != nil {
		return nil, fmt.Errorf("scan workspaces: %w", err)
	}
	return workspaces, nil
}

func (r *Repository) WorkspaceForUser(ctx context.Context, workspaceID, userID uuid.UUID) (domain.UserWorkspace, error) {
	const query = `
		SELECT w.id, w.name, w.created_at, m.role
		FROM memberships m
		JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.workspace_id = $1 AND m.user_id = $2`

	rows, err := r.pool.Query(ctx, query, workspaceID, userID)
	if err != nil {
		return domain.UserWorkspace{}, fmt.Errorf("select membership: %w", err)
	}
	workspace, err := pgx.CollectExactlyOneRow(rows, scanUserWorkspace)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.UserWorkspace{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.UserWorkspace{}, fmt.Errorf("scan membership: %w", err)
	}
	return workspace, nil
}

func scanUserWorkspace(row pgx.CollectableRow) (domain.UserWorkspace, error) {
	var uw domain.UserWorkspace
	err := row.Scan(&uw.Workspace.ID, &uw.Workspace.Name, &uw.Workspace.CreatedAt, &uw.Role)
	return uw, err
}

func (r *Repository) Members(ctx context.Context, workspaceID uuid.UUID) ([]domain.Member, error) {
	const query = `
		SELECT u.id, u.email, u.display_name, m.role, m.created_at
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = $1
		ORDER BY u.display_name, u.id`

	rows, err := r.pool.Query(ctx, query, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("select members: %w", err)
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Member, error) {
		var m domain.Member
		err := row.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.JoinedAt)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan members: %w", err)
	}
	return members, nil
}

func (r *Repository) IsMemberEmail(ctx context.Context, workspaceID uuid.UUID, email string) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1
			FROM memberships m
			JOIN users u ON u.id = m.user_id
			WHERE m.workspace_id = $1 AND u.email = $2
		)`

	var exists bool
	if err := r.pool.QueryRow(ctx, query, workspaceID, email).Scan(&exists); err != nil {
		return false, fmt.Errorf("check member email: %w", err)
	}
	return exists, nil
}

// ChangeRole updates a member's role, refusing to demote the last admin.
func (r *Repository) ChangeRole(ctx context.Context, workspaceID, userID uuid.UUID, newRole domain.Role) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		currentRole, adminCount, err := lockMember(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if err := domain.CheckAdminRemains(adminCount, currentRole, newRole); err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`UPDATE memberships SET role = $3 WHERE workspace_id = $1 AND user_id = $2`,
			workspaceID, userID, newRole,
		)
		if err != nil {
			return fmt.Errorf("update role: %w", err)
		}
		return nil
	})
}

// RemoveMember deletes a membership, refusing to remove the last admin. Because roles are
// read from the database on every request, the removed user loses access immediately,
// even with a session that is still valid.
func (r *Repository) RemoveMember(ctx context.Context, workspaceID, userID uuid.UUID) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		currentRole, adminCount, err := lockMember(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if err := domain.CheckAdminRemains(adminCount, currentRole, ""); err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`DELETE FROM memberships WHERE workspace_id = $1 AND user_id = $2`,
			workspaceID, userID,
		)
		if err != nil {
			return fmt.Errorf("delete membership: %w", err)
		}
		return nil
	})
}

// lockMember locks the workspace row so concurrent role changes run one at a time, then
// returns the member's current role and the number of admins. Without the lock, two admins
// demoting each other at the same moment could both see "2 admins" and leave none.
func lockMember(ctx context.Context, tx pgx.Tx, workspaceID, userID uuid.UUID) (domain.Role, int, error) {
	var lockedID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, domain.ErrNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("lock workspace: %w", err)
	}

	var currentRole domain.Role
	err = tx.QueryRow(ctx,
		`SELECT role FROM memberships WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID,
	).Scan(&currentRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, domain.ErrNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("select member role: %w", err)
	}

	var adminCount int
	err = tx.QueryRow(ctx,
		`SELECT count(*) FROM memberships WHERE workspace_id = $1 AND role = $2`,
		workspaceID, domain.RoleAdmin,
	).Scan(&adminCount)
	if err != nil {
		return "", 0, fmt.Errorf("count admins: %w", err)
	}
	return currentRole, adminCount, nil
}

// CreateInvitation stores a new invitation. Expired invitations for the same email are
// closed first: the "one open invitation per email" index cannot see expiry, because
// now() is not allowed in an index predicate.
func (r *Repository) CreateInvitation(ctx context.Context, invitation domain.Invitation) (domain.Invitation, error) {
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE invitations
			SET revoked_at = now()
			WHERE workspace_id = $1 AND email = $2
			  AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at <= now()`,
			invitation.WorkspaceID, invitation.Email,
		)
		if err != nil {
			return fmt.Errorf("close expired invitations: %w", err)
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO invitations (workspace_id, email, role, token_hash, invited_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, created_at`,
			invitation.WorkspaceID, invitation.Email, invitation.Role,
			invitation.TokenHash, invitation.InvitedBy, invitation.ExpiresAt,
		).Scan(&invitation.ID, &invitation.CreatedAt)
		if postgres.IsUniqueViolation(err, "invitations_open_email_idx") {
			return domain.ErrInvitationPending
		}
		if err != nil {
			return fmt.Errorf("insert invitation: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Invitation{}, err
	}
	return invitation, nil
}

const invitationColumns = `id, workspace_id, email, role, token_hash, invited_by,
	created_at, expires_at, accepted_at, revoked_at`

func scanInvitation(row pgx.CollectableRow) (domain.Invitation, error) {
	var inv domain.Invitation
	var invitedBy uuid.NullUUID // NULL when the inviting user was deleted
	err := row.Scan(&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.TokenHash, &invitedBy,
		&inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt)
	inv.InvitedBy = invitedBy.UUID
	return inv, err
}

func (r *Repository) OpenInvitations(ctx context.Context, workspaceID uuid.UUID, now time.Time) ([]domain.Invitation, error) {
	query := `
		SELECT ` + invitationColumns + `
		FROM invitations
		WHERE workspace_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $2
		ORDER BY created_at DESC`

	rows, err := r.pool.Query(ctx, query, workspaceID, now)
	if err != nil {
		return nil, fmt.Errorf("select invitations: %w", err)
	}
	invitations, err := pgx.CollectRows(rows, scanInvitation)
	if err != nil {
		return nil, fmt.Errorf("scan invitations: %w", err)
	}
	return invitations, nil
}

func (r *Repository) InvitationByTokenHash(ctx context.Context, tokenHash []byte) (domain.Invitation, error) {
	query := `SELECT ` + invitationColumns + ` FROM invitations WHERE token_hash = $1`

	rows, err := r.pool.Query(ctx, query, tokenHash)
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("select invitation: %w", err)
	}
	invitation, err := pgx.CollectExactlyOneRow(rows, scanInvitation)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Invitation{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("scan invitation: %w", err)
	}
	return invitation, nil
}

// AcceptInvitation marks the invitation accepted and creates the membership in one
// transaction. The UPDATE re-checks that the invitation is still open, so two concurrent
// accepts of the same link cannot both succeed.
func (r *Repository) AcceptInvitation(ctx context.Context, invitationID, userID uuid.UUID, now time.Time) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var workspaceID uuid.UUID
		var role domain.Role
		err := tx.QueryRow(ctx, `
			UPDATE invitations
			SET accepted_at = $2
			WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $2
			RETURNING workspace_id, role`,
			invitationID, now,
		).Scan(&workspaceID, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvitationInvalid
		}
		if err != nil {
			return fmt.Errorf("mark invitation accepted: %w", err)
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			workspaceID, userID, role,
		)
		if postgres.IsUniqueViolation(err, "memberships_pkey") {
			return domain.ErrAlreadyMember
		}
		if err != nil {
			return fmt.Errorf("insert membership: %w", err)
		}
		return nil
	})
}

func (r *Repository) RevokeInvitation(ctx context.Context, workspaceID, invitationID uuid.UUID, now time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE invitations
		SET revoked_at = $3
		WHERE workspace_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL`,
		workspaceID, invitationID, now,
	)
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
