// Package app implements the workspace use cases. Every use case starts by loading the
// caller's membership: non-members get ErrNotFound and members without the needed role
// get ErrForbidden. Access is denied unless a rule explicitly allows it.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"enclave/api/internal/platform/authn"
	"enclave/api/internal/platform/email"
	"enclave/api/internal/platform/securetoken"
	"enclave/api/internal/workspace/domain"
)

const (
	invitationTTL          = 7 * 24 * time.Hour
	maxWorkspaceNameLength = 100
)

// Repository is the storage the use cases need, declared next to its consumer (ADR 0001).
// Methods that must check and change data together (role changes, accepting invitations)
// run in one transaction inside the repository.
type Repository interface {
	CreateWorkspace(ctx context.Context, name string, adminID uuid.UUID) (domain.Workspace, error)
	WorkspacesForUser(ctx context.Context, userID uuid.UUID) ([]domain.UserWorkspace, error)
	WorkspaceForUser(ctx context.Context, workspaceID, userID uuid.UUID) (domain.UserWorkspace, error)
	Members(ctx context.Context, workspaceID uuid.UUID) ([]domain.Member, error)
	IsMemberEmail(ctx context.Context, workspaceID uuid.UUID, email string) (bool, error)
	ChangeRole(ctx context.Context, workspaceID, userID uuid.UUID, newRole domain.Role) error
	RemoveMember(ctx context.Context, workspaceID, userID uuid.UUID) error

	CreateInvitation(ctx context.Context, invitation domain.Invitation) (domain.Invitation, error)
	OpenInvitations(ctx context.Context, workspaceID uuid.UUID, now time.Time) ([]domain.Invitation, error)
	InvitationByTokenHash(ctx context.Context, tokenHash []byte) (domain.Invitation, error)
	AcceptInvitation(ctx context.Context, invitationID, userID uuid.UUID, now time.Time) error
	RevokeInvitation(ctx context.Context, workspaceID, invitationID uuid.UUID, now time.Time) error
}

type Service struct {
	repo Repository
	now  func() time.Time
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: time.Now}
}

// CreateWorkspace creates a workspace with the caller as its first admin.
func (s *Service) CreateWorkspace(ctx context.Context, caller authn.Principal, name string) (domain.UserWorkspace, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxWorkspaceNameLength {
		return domain.UserWorkspace{}, invalidInput(fmt.Sprintf("name must be 1-%d characters", maxWorkspaceNameLength))
	}

	workspace, err := s.repo.CreateWorkspace(ctx, name, caller.UserID)
	if err != nil {
		return domain.UserWorkspace{}, fmt.Errorf("create workspace: %w", err)
	}
	return domain.UserWorkspace{Workspace: workspace, Role: domain.RoleAdmin}, nil
}

func (s *Service) ListWorkspaces(ctx context.Context, caller authn.Principal) ([]domain.UserWorkspace, error) {
	workspaces, err := s.repo.WorkspacesForUser(ctx, caller.UserID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces for user %s: %w", caller.UserID, err)
	}
	return workspaces, nil
}

func (s *Service) GetWorkspace(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) (domain.UserWorkspace, error) {
	return s.membership(ctx, caller, workspaceID)
}

func (s *Service) ListMembers(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) ([]domain.Member, error) {
	membership, err := s.membership(ctx, caller, workspaceID)
	if err != nil {
		return nil, err
	}
	if !membership.Role.CanViewMembers() {
		return nil, domain.ErrForbidden
	}

	members, err := s.repo.Members(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list members of workspace %s: %w", workspaceID, err)
	}
	return members, nil
}

func (s *Service) ChangeRole(ctx context.Context, caller authn.Principal, workspaceID, userID uuid.UUID, rawRole string) error {
	if err := s.requireMemberManager(ctx, caller, workspaceID); err != nil {
		return err
	}
	role, ok := domain.ParseRole(rawRole)
	if !ok {
		return invalidInput("role must be admin, staff or client")
	}

	if err := s.repo.ChangeRole(ctx, workspaceID, userID, role); err != nil {
		return fmt.Errorf("change role of user %s in workspace %s: %w", userID, workspaceID, err)
	}
	return nil
}

func (s *Service) RemoveMember(ctx context.Context, caller authn.Principal, workspaceID, userID uuid.UUID) error {
	if err := s.requireMemberManager(ctx, caller, workspaceID); err != nil {
		return err
	}
	if err := s.repo.RemoveMember(ctx, workspaceID, userID); err != nil {
		return fmt.Errorf("remove user %s from workspace %s: %w", userID, workspaceID, err)
	}
	return nil
}

type InviteResult struct {
	Invitation domain.Invitation
	// Token is the secret for the invite link. It is returned once and never stored.
	Token string
}

func (s *Service) Invite(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID, rawEmail, rawRole string) (InviteResult, error) {
	if err := s.requireMemberManager(ctx, caller, workspaceID); err != nil {
		return InviteResult{}, err
	}
	address, ok := email.Normalize(rawEmail)
	if !ok {
		return InviteResult{}, invalidInput("email is not a valid address")
	}
	role, ok := domain.ParseRole(rawRole)
	if !ok {
		return InviteResult{}, invalidInput("role must be admin, staff or client")
	}

	isMember, err := s.repo.IsMemberEmail(ctx, workspaceID, address)
	if err != nil {
		return InviteResult{}, fmt.Errorf("check existing member: %w", err)
	}
	if isMember {
		return InviteResult{}, domain.ErrAlreadyMember
	}

	token, tokenHash := securetoken.New()
	invitation, err := s.repo.CreateInvitation(ctx, domain.Invitation{
		WorkspaceID: workspaceID,
		Email:       address,
		Role:        role,
		TokenHash:   tokenHash,
		InvitedBy:   caller.UserID,
		ExpiresAt:   s.now().Add(invitationTTL),
	})
	if err != nil {
		return InviteResult{}, fmt.Errorf("create invitation: %w", err)
	}
	return InviteResult{Invitation: invitation, Token: token}, nil
}

func (s *Service) ListInvitations(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) ([]domain.Invitation, error) {
	if err := s.requireMemberManager(ctx, caller, workspaceID); err != nil {
		return nil, err
	}
	invitations, err := s.repo.OpenInvitations(ctx, workspaceID, s.now())
	if err != nil {
		return nil, fmt.Errorf("list invitations of workspace %s: %w", workspaceID, err)
	}
	return invitations, nil
}

func (s *Service) RevokeInvitation(ctx context.Context, caller authn.Principal, workspaceID, invitationID uuid.UUID) error {
	if err := s.requireMemberManager(ctx, caller, workspaceID); err != nil {
		return err
	}
	if err := s.repo.RevokeInvitation(ctx, workspaceID, invitationID, s.now()); err != nil {
		return fmt.Errorf("revoke invitation %s: %w", invitationID, err)
	}
	return nil
}

// AcceptInvitation adds the caller to the invitation's workspace. The caller must be logged
// in with the email the invitation was sent to.
func (s *Service) AcceptInvitation(ctx context.Context, caller authn.Principal, token string) (domain.UserWorkspace, error) {
	if token == "" {
		return domain.UserWorkspace{}, domain.ErrInvitationInvalid
	}

	invitation, err := s.repo.InvitationByTokenHash(ctx, securetoken.Hash(token))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.UserWorkspace{}, domain.ErrInvitationInvalid
	}
	if err != nil {
		return domain.UserWorkspace{}, fmt.Errorf("load invitation: %w", err)
	}
	if !invitation.IsOpen(s.now()) {
		return domain.UserWorkspace{}, domain.ErrInvitationInvalid
	}
	if invitation.Email != caller.Email {
		return domain.UserWorkspace{}, domain.ErrInvitationEmailMismatch
	}

	if err := s.repo.AcceptInvitation(ctx, invitation.ID, caller.UserID, s.now()); err != nil {
		return domain.UserWorkspace{}, fmt.Errorf("accept invitation %s: %w", invitation.ID, err)
	}
	return s.membership(ctx, caller, invitation.WorkspaceID)
}

// membership returns the caller's view of the workspace, or ErrNotFound if the caller is
// not a member.
func (s *Service) membership(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) (domain.UserWorkspace, error) {
	membership, err := s.repo.WorkspaceForUser(ctx, workspaceID, caller.UserID)
	if err != nil {
		return domain.UserWorkspace{}, fmt.Errorf("load membership in workspace %s: %w", workspaceID, err)
	}
	return membership, nil
}

func (s *Service) requireMemberManager(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) error {
	membership, err := s.membership(ctx, caller, workspaceID)
	if err != nil {
		return err
	}
	if !membership.Role.CanManageMembers() {
		return domain.ErrForbidden
	}
	return nil
}

func invalidInput(message string) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidInput, message)
}
