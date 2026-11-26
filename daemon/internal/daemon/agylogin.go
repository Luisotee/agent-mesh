package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// agyLoginRetry is how long meshd leaves agy alone after a probe of it tried to open a login, unless agy's token
// file changes (the user logged in): a probe that wants a login wants it again at the next try.
const agyLoginRetry = 30 * time.Minute

// agyLoginNote is the line `mesh usage` shows while agy needs a login.
const agyLoginNote = "agy needs `/login` (run `agy`, then `/login`): its usage and models can't be read until it is logged in"

// agyTokenSig is a signature of agy's token file (its modification time and size, never its content): a login
// or a refresh changes it.
func (d *Daemon) agyTokenSig() string {
	home := d.usageEnv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	st, err := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	if err != nil {
		return ""
	}
	return strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10)
}

// noteAgyLogin records that a probe of agy tried to open a login.
func (d *Daemon) noteAgyLogin() {
	path := d.paths.AgyLoginNeeded()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = fsutil.WriteAtomic(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"+d.agyTokenSig()), 0o600)
	d.log.Warn("agy needs a login: a probe of it tried to open a browser; not probing it again for a while")
}

func (d *Daemon) clearAgyLogin() { _ = os.Remove(d.paths.AgyLoginNeeded()) }

// agyLoginState reads the record: when the probe tried, and whether meshd should still leave agy alone (it is
// recent and the token file is as it was).
func (d *Daemon) agyLoginState(now time.Time) (since time.Time, hold bool) {
	b, err := os.ReadFile(d.paths.AgyLoginNeeded())
	if err != nil {
		return time.Time{}, false
	}
	first, sig, _ := strings.Cut(string(b), "\n")
	since, err = time.Parse(time.RFC3339, strings.TrimSpace(first))
	if err != nil {
		return time.Time{}, false
	}
	return since, now.Sub(since) < agyLoginRetry && sig == d.agyTokenSig()
}

// agyLoginAlertPass tells the root of each open run that has an agy agent that agy needs a login, once per
// record (and again a day later while it still does): the agents' own turns fail the same way.
func (d *Daemon) agyLoginAlertPass(ctx context.Context) {
	now := d.now()
	since, _ := d.agyLoginState(now)
	if since.IsZero() {
		return
	}
	r := d.store.Read(ctx)
	agents, err := r.ListAgents("")
	if err != nil {
		return
	}
	leads := map[string]*v1.Agent{}
	usesAgy := map[string]bool{}
	for _, a := range agents {
		if stopped(a) {
			continue
		}
		if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
			leads[a.RunId] = a
		} else if a.Kind == "agy" {
			usesAgy[a.RunId] = true
		}
	}
	for runID, lead := range leads {
		if !usesAgy[runID] {
			continue
		}
		key := "login-agy-" + since.UTC().Format(time.RFC3339)
		if !d.shouldSendReserveAlert(runID, key, now) {
			continue
		}
		body := fmt.Sprintf("[mesh alert login agy] %s. Tell the user: agy workers can't start a turn until it is logged in. "+
			"Nothing for you to send; the daemon looks again by itself once the login is done.", agyLoginNote)
		var delivered bool
		err := d.store.Write(ctx, meshdActor, "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(lead.Id)
			if err != nil || stopped(cur) {
				return nil
			}
			if _, err := storeMessage(tx, cur, v1.MessageType_MESSAGE_TYPE_INFO, body); err != nil {
				return err
			}
			delivered = true
			return nil
		})
		if err == nil && delivered {
			d.markReserveAlertSent(runID, key, now.Add(24*time.Hour))
			d.deliverSoon(lead.Id)
		}
	}
}
