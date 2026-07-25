//go:build runtime_integration

package main

// runtimeIntegrationBuild is compiled only into disposable integration-test
// binaries. Release and ordinary local builds cannot enable these concessions.
const runtimeIntegrationBuild = true

func integrationSharedAgentRoleAllowed() bool {
	return envBool("RUNTIME_TEST_ALLOW_SHARED_AGENT_DB_ROLE")
}

func integrationAgentRoleRebindAllowed() bool {
	return envBool("RUNTIME_TEST_ALLOW_AGENT_ROLE_REBIND")
}
