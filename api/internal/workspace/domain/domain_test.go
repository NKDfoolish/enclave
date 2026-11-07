package domain

import (
	"errors"
	"testing"
	"time"
)

func TestParseRole(t *testing.T) {
	for _, valid := range []string{"admin", "staff", "client"} {
		if _, ok := ParseRole(valid); !ok {
			t.Errorf("ParseRole(%q) should succeed", valid)
		}
	}
	for _, invalid := range []string{"", "owner", "Admin"} {
		if _, ok := ParseRole(invalid); ok {
			t.Errorf("ParseRole(%q) should fail", invalid)
		}
	}
}

func TestRolePermissions(t *testing.T) {
	tests := []struct {
		role             Role
		canManageMembers bool
		canViewMembers   bool
	}{
		{RoleAdmin, true, true},
		{RoleStaff, false, true},
		{RoleClient, false, false},
	}
	for _, tc := range tests {
		if got := tc.role.CanManageMembers(); got != tc.canManageMembers {
			t.Errorf("%s.CanManageMembers() = %v, want %v", tc.role, got, tc.canManageMembers)
		}
		if got := tc.role.CanViewMembers(); got != tc.canViewMembers {
			t.Errorf("%s.CanViewMembers() = %v, want %v", tc.role, got, tc.canViewMembers)
		}
	}
}

func TestCheckAdminRemains(t *testing.T) {
	tests := []struct {
		name        string
		adminCount  int
		currentRole Role
		newRole     Role
		wantErr     error
	}{
		{"demote the only admin", 1, RoleAdmin, RoleStaff, ErrLastAdmin},
		{"remove the only admin", 1, RoleAdmin, "", ErrLastAdmin},
		{"demote one of two admins", 2, RoleAdmin, RoleClient, nil},
		{"admin stays admin", 1, RoleAdmin, RoleAdmin, nil},
		{"change a staff member", 1, RoleStaff, RoleClient, nil},
		{"remove a client", 1, RoleClient, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckAdminRemains(tc.adminCount, tc.currentRole, tc.newRole)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestInvitationIsOpen(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)

	tests := []struct {
		name       string
		invitation Invitation
		want       bool
	}{
		{"pending", Invitation{ExpiresAt: now.Add(time.Hour)}, true},
		{"expired", Invitation{ExpiresAt: past}, false},
		{"accepted", Invitation{ExpiresAt: now.Add(time.Hour), AcceptedAt: &past}, false},
		{"revoked", Invitation{ExpiresAt: now.Add(time.Hour), RevokedAt: &past}, false},
	}
	for _, tc := range tests {
		if got := tc.invitation.IsOpen(now); got != tc.want {
			t.Errorf("%s: IsOpen = %v, want %v", tc.name, got, tc.want)
		}
	}
}
