// Package statedir names the files in a daemon's state dir, which the daemon, the CLI and the hook command
// all read: the socket, meshd.json, tokens, and the hook spool.
package statedir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Paths of the files in a state dir.
type Paths struct{ Dir string }

func (p Paths) Lock() string        { return filepath.Join(p.Dir, "meshd.lock") }
func (p Paths) Socket() string      { return filepath.Join(p.Dir, "meshd.sock") }
func (p Paths) Info() string        { return filepath.Join(p.Dir, "meshd.json") }
func (p Paths) DB() string          { return filepath.Join(p.Dir, "meshd.db") }
func (p Paths) DeviceToken() string { return filepath.Join(p.Dir, "device.token") }
func (p Paths) DaemonID() string    { return filepath.Join(p.Dir, "daemon.id") }
func (p Paths) TLSCert() string     { return filepath.Join(p.Dir, "tls.crt") }
func (p Paths) TLSKey() string      { return filepath.Join(p.Dir, "tls.key") }
func (p Paths) HookLog() string     { return filepath.Join(p.Dir, "hook.log") }

// Term is the terminal host's dir.
func (p Paths) Term() string { return filepath.Join(p.Dir, "term") }

// Worktrees is where agents' worktrees go unless the daemon is told otherwise.
func (p Paths) Worktrees() string { return filepath.Join(p.Dir, "worktrees") }

// RunDir is a run's own folder, where its agents write plans, reports and tasks (and the root its notes): the
// protocol's `$(mesh dir)`. The daemon makes it with the run, and names it to every agent in $AGENT_MESH_RUN_DIR.
func (p Paths) RunDir(run string) string { return filepath.Join(p.Dir, "runs", run) }

// RunFolders are the folders every run's dir holds.
var RunFolders = []string{"plans", "reports", "tasks"}

// Agent is an agent's own dir: its token for the hook command.
func (p Paths) Agent(id string) string      { return filepath.Join(p.Dir, "agents", id) }
func (p Paths) AgentToken(id string) string { return filepath.Join(p.Agent(id), "token") }

// Spool is where an agent's hooks wait while the daemon is down.
func (p Paths) Spool(id string) string { return filepath.Join(p.Dir, "spool", id) }
func (p Paths) SpoolRoot() string      { return filepath.Join(p.Dir, "spool") }

// Info is meshd.json: how clients and hooks find the daemon, read at call time.
type Info struct {
	Socket          string    `json:"socket"`
	TCP             string    `json:"tcp,omitempty"`
	Listen          string    `json:"listen,omitempty"`
	CertFingerprint string    `json:"cert_fingerprint,omitempty"`
	PID             int       `json:"pid"`
	DaemonID        string    `json:"daemon_id"`
	InstanceID      string    `json:"instance_id"`
	StartedAt       time.Time `json:"started_at"`
	Version         string    `json:"version"`
}

// ReadInfo reads a state dir's meshd.json.
func ReadInfo(dir string) (Info, error) {
	var info Info
	b, err := os.ReadFile(Paths{Dir: dir}.Info())
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}
