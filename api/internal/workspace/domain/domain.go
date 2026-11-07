// Package domain holds the workspace model: workspaces, members with workspace-scoped
// roles, invitations, and the rules about who may do what.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Role belongs to a membership, not to a user: the same person can be an admin in one
// workspace and a client in another.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleStaff  Role = "staff"
	RoleClient Role = "client"
)

func ParseRole(s string) (Role, bool) {
	switch role := Role(s); role {
	case RoleAdmin, RoleStaff, RoleClient:
		return role, true
	default:
		return "", false
	}
}

// CanManageMembers covers inviting, changing roles and removing members.
func (r Role) CanManageMembers() bool {
	return r == RoleAdmin
}

// CanViewMembers: clients see the workspace but not who else belongs to it.
func (r Role) CanViewMembers() bool {
	return r == RoleAdmin || r == RoleStaff
}

type Workspace struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
}

// UserWorkspace is a workspace as seen by one member, with that member's role.
type UserWorkspace struct {
	Workspace Workspace
	Role      Role
}

type Member struct {
	UserID      uuid.UUID
	Email       string
	DisplayName string
	Role        Role
	JoinedAt    time.Time
}

type Invitation struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Email       string
	Role        Role
	TokenHash   []byte
	InvitedBy   uuid.UUID
	CreatedAt   time.Time
	ExpiresAt   time.Time
	AcceptedAt  *time.Time
	RevokedAt   *time.Time
}

// IsOpen reports whether the invitation can still be accepted.
func (i Invitation) IsOpen(now time.Time) bool {
	return i.AcceptedAt == nil && i.RevokedAt == nil && now.Before(i.ExpiresAt)
}

// CheckAdminRemains returns ErrLastAdmin if changing a member from currentRole to newRole
// would leave the workspace with no admin. adminCount is the number of admins before the
// change. Pass an empty newRole when the member is being removed.
func CheckAdminRemains(adminCount int, currentRole, newRole Role) error {
	if currentRole == RoleAdmin && newRole != RoleAdmin && adminCount <= 1 {
		return ErrLastAdmin
	}
	return nil
}

var (
	ErrInvalidInput = errors.New("invalid input")
	// ErrNotFound is also returned when the caller is not a member, so outsiders cannot
	// tell whether a workspace exists.
	ErrNotFound                = errors.New("not found")
	ErrForbidden               = errors.New("forbidden")
	ErrAlreadyMember           = errors.New("user is already a member")
	ErrInvitationPending       = errors.New("an invitation for this email is already pending")
	ErrInvitationInvalid       = errors.New("invitation is invalid, expired or already used")
	ErrInvitationEmailMismatch = errors.New("invitation was sent to a different email")
	ErrLastAdmin               = errors.New("workspace must keep at least one admin")
)
