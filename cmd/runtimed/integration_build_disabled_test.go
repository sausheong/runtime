//go:build !runtime_integration

package main

import "testing"

func TestProductionBuildCannotEnableIntegrationRoleBypasses(t *testing.T) {
	t.Setenv("RUNTIME_TEST_ALLOW_SHARED_AGENT_DB_ROLE", "1")
	t.Setenv("RUNTIME_TEST_ALLOW_AGENT_ROLE_REBIND", "1")
	if runtimeIntegrationBuild {
		t.Fatal("ordinary build unexpectedly enables integration database-role bypasses")
	}
	if integrationSharedAgentRoleAllowed() || integrationAgentRoleRebindAllowed() {
		t.Fatal("production build accepted integration database-role environment variables")
	}
}
