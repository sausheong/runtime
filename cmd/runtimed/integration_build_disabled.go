//go:build !runtime_integration

package main

// runtimeIntegrationBuild keeps test-only database-role concessions out of
// every production build, even if their old environment variables are set.
const runtimeIntegrationBuild = false

func integrationSharedAgentRoleAllowed() bool { return false }
func integrationAgentRoleRebindAllowed() bool { return false }
