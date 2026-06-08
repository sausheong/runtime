package identity

import "testing"

func newTestAuthorizer() *Authorizer {
	// agent "a1" belongs to tenant "alpha"; "b1" to "beta".
	return NewAuthorizer(map[string]string{"a1": "alpha", "b1": "beta"})
}

func TestAuthorize_RoleActionMatrix(t *testing.T) {
	az := newTestAuthorizer()
	cases := []struct {
		role    Role
		action  Action
		allowed bool
	}{
		{RoleViewer, ActionRead, true},
		{RoleViewer, ActionInvoke, false},
		{RoleViewer, ActionAdmin, false},
		{RoleOperator, ActionRead, true},
		{RoleOperator, ActionInvoke, true},
		{RoleOperator, ActionAdmin, false},
		{RoleAdmin, ActionRead, true},
		{RoleAdmin, ActionInvoke, true},
		{RoleAdmin, ActionAdmin, true},
	}
	for _, c := range cases {
		p := Principal{TenantID: "alpha", Subject: "s", Role: c.role}
		err := az.Authorize(p, "a1", c.action)
		if (err == nil) != c.allowed {
			t.Errorf("role=%s action=%s: allowed=%v, err=%v", c.role, c.action, c.allowed, err)
		}
	}
}

func TestAuthorize_CrossTenantIsNotFound(t *testing.T) {
	az := newTestAuthorizer()
	p := Principal{TenantID: "alpha", Subject: "s", Role: RoleAdmin}
	err := az.Authorize(p, "b1", ActionRead) // b1 is beta's
	if err != ErrNotFound {
		t.Fatalf("cross-tenant: err=%v, want ErrNotFound", err)
	}
}

func TestAuthorize_UnknownAgentIsNotFound(t *testing.T) {
	az := newTestAuthorizer()
	p := Principal{TenantID: "alpha", Subject: "s", Role: RoleAdmin}
	if err := az.Authorize(p, "ghost", ActionRead); err != ErrNotFound {
		t.Fatalf("unknown agent: err=%v, want ErrNotFound", err)
	}
}

func TestAuthorize_ForbiddenWhenRoleTooLow(t *testing.T) {
	az := newTestAuthorizer()
	p := Principal{TenantID: "alpha", Subject: "s", Role: RoleViewer}
	if err := az.Authorize(p, "a1", ActionInvoke); err != ErrForbidden {
		t.Fatalf("viewer invoke: err=%v, want ErrForbidden", err)
	}
}

func TestAuthorize_SuperuserCrossTenantAllowed(t *testing.T) {
	az := newTestAuthorizer()
	p := Principal{Subject: "bootstrap", Role: RoleAdmin, Superuser: true}
	if err := az.Authorize(p, "b1", ActionRead); err != nil {
		t.Fatalf("superuser cross-tenant: err=%v, want nil", err)
	}
}

func TestVisibleAgents_FiltersByTenant(t *testing.T) {
	az := newTestAuthorizer()
	p := Principal{TenantID: "alpha", Role: RoleViewer}
	got := az.AgentTenant("a1")
	if got != "alpha" {
		t.Fatalf("AgentTenant(a1)=%q want alpha", got)
	}
	if !az.CanSeeAgent(p, "a1") || az.CanSeeAgent(p, "b1") {
		t.Fatalf("alpha viewer should see a1 not b1")
	}
}
