// Package cleanenv drops the agent identity from a test process's environment. A test run by a daemon agent
// (an agent's shell carries AGENT_MESH_TOKEN, AGENT_MESH_STATE and friends) would otherwise present that
// agent's token to the throwaway daemon the test starts: "unknown or revoked token".
package cleanenv

import (
	"os"
	"strings"
)

// Agent unsets every AGENT_MESH_* variable. Call it first in a TestMain.
func Agent() {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "AGENT_MESH_") {
			os.Unsetenv(name)
		}
	}
}
