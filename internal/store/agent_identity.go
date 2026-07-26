package store

import (
	"crypto/sha256"
	"fmt"
)

// AgentDBOSSchema returns the stable, identifier-safe DBOS schema allocated to
// one tenant/agent trust domain. The source identifiers are hashed so operator
// input never becomes SQL syntax and the result stays well below PostgreSQL's
// 63-byte identifier limit.
func AgentDBOSSchema(tenantID, agentID string) string {
	sum := sha256.Sum256([]byte(tenantID + "/" + agentID))
	return fmt.Sprintf("dbos_agent_%x", sum[:12])
}
