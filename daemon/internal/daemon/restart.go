package daemon

// Resuming a run after a machine restart. The terminal host dies with the machine (a WSL restart, a reboot), so at
// boot `adopt` finds every running agent's terminal gone and records it `lost`; before this, the user resumed the root
// and each worker by hand, and a review round whose member was lost finished "0/2 answered" with nobody told.
//
// Now `adopt` picks the agents that were live (wantsBootResume) and, once the daemon serves, resumeAfterRestart brings
// them back in their own conversations, root first, then workers, then the members of open asks, a few seconds apart so
// ten launches (and the compactions a cold resume may start) do not begin at once. What a guard refuses (the host's cap,
// the memory floor, the quota reserve) is tried again for a while, since memory is tight just after a boot; what is
// still refused then is told to the parent as the exit alert the agent would have had (`exited`: an ask member
// settles `lost` and its asker hears).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// restartTag starts the info a resumed agent is told.
const restartTag = "[mesh restart]"

// The defaults of AgentConfig's RestartGap, RestartRetry and RestartWindow.
const (
	restartGap    = 5 * time.Second
	restartRetry  = 30 * time.Second
	restartWindow = 10 * time.Minute
)

// bootResumes is the agents recorded lost at this boot that a resume is wanted for, in the order they are brought back.
type bootResumes struct {
	ids  []string
	rank map[string]int
	skip map[string]bool // told the parent nothing yet: the exit alert waits for the resume's outcome
}

// wantsBootResume reports whether an agent whose terminal is gone at boot should be brought back, and its place in the
// order (0 the root, 1 a worker, 2 a member of an open ask). A worker that had reported (a `done` since its last task)
// stays stopped: a later message resumes it. Nothing is brought back that the daemon may not resume: an adopted session
// of the user's, a closed run, an agent with no conversation.
func (d *Daemon) wantsBootResume(r store.Reader, a *v1.Agent) (bool, int) {
	if on, err := d.Setting(r, "restart.resume"); err != nil || on != true {
		return false, 0
	}
	if isAdopted(a) || a.SessionId == "" || a.State == v1.AgentState_AGENT_STATE_RETIRED {
		return false, 0
	}
	if run, err := r.GetRun(a.RunId); err != nil || run.Status != v1.RunStatus_RUN_STATUS_OPEN {
		return false, 0
	}
	if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
		return true, 0
	}
	if report, err := lastReport(r, a); err != nil || report != nil {
		return false, 0
	}
	if asks, err := r.OpenAsks(); err == nil {
		for _, ask := range asks {
			for _, m := range ask.Members {
				if m.AgentId == a.Id && m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING {
					return true, 2
				}
			}
		}
	}
	if a.Peer {
		return false, 0 // a standing asker is brought back by the ask that wants it
	}
	return true, 1
}

// noteBootResume records an agent lost at this boot that will be resumed: its exit alert is held (alertExitTx).
func (d *Daemon) noteBootResume(a *v1.Agent, rank int) {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	if d.rt.boot.rank == nil {
		d.rt.boot.rank, d.rt.boot.skip = map[string]int{}, map[string]bool{}
	}
	d.rt.boot.ids = append(d.rt.boot.ids, a.Id)
	d.rt.boot.rank[a.Id] = rank
	d.rt.boot.skip[a.Id] = true
}

// bootHeld reports whether an agent's exit alert is held for a resume that has not ended.
func (d *Daemon) bootHeld(id string) bool {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	return d.rt.boot.skip[id]
}

func (d *Daemon) bootRelease(id string) {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	delete(d.rt.boot.skip, id)
}

// resumeAfterRestart brings back the agents `adopt` recorded: once each, spaced, then the refused ones again until the
// window ends. It runs in the background after the daemon serves.
func (d *Daemon) resumeAfterRestart(ctx context.Context) {
	d.rt.mu.Lock()
	ids := append([]string(nil), d.rt.boot.ids...)
	rank := d.rt.boot.rank
	d.rt.boot.ids = nil
	d.rt.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	sort.SliceStable(ids, func(i, j int) bool { return rank[ids[i]] < rank[ids[j]] })
	gap, retry, window := orDuration(d.rt.cfg.RestartGap, restartGap), orDuration(d.rt.cfg.RestartRetry, restartRetry),
		orDuration(d.rt.cfg.RestartWindow, restartWindow)
	sleep := func(t time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(t):
			return true
		}
	}
	// What is not resumed when the daemon stops is told as lost now: the hold on its alert lives in memory, and a daemon
	// started again finds it stopped, with nobody left to tell the parent.
	left := map[string]bool{}
	for _, id := range ids {
		left[id] = true
	}
	defer func() {
		if ctx.Err() == nil {
			return
		}
		for _, id := range ids {
			if left[id] {
				d.lostAfterRestart(context.WithoutCancel(ctx), id, errors.New("meshd stopped before it could be resumed"))
			}
		}
	}()
	refused := map[string]error{}
	var order []string
	for i, id := range ids {
		if i > 0 && !sleep(gap) {
			return
		}
		if err := d.resumeLost(ctx, id); err != nil {
			d.log.Info("resume after a restart refused", "agent", id, "err", err)
			refused[id] = err
			order = append(order, id)
			continue
		}
		left[id] = false
		d.bootRelease(id)
	}
	for end := d.now().Add(window); len(refused) > 0 && d.now().Before(end); {
		if !sleep(retry) {
			return
		}
		for _, id := range order {
			if _, ok := refused[id]; !ok {
				continue
			}
			if err := d.resumeLost(ctx, id); err != nil {
				refused[id] = err
				continue
			}
			delete(refused, id)
			left[id] = false
			d.bootRelease(id)
		}
	}
	for _, id := range order {
		if err, ok := refused[id]; ok {
			left[id] = false
			d.lostAfterRestart(ctx, id, err)
		}
	}
}

// restartInfo is what a resumed agent is told: what died with the machine.
func restartInfo(a *v1.Agent, ask *v1.Ask) string {
	text := restartTag + " The machine (or meshd's terminal host) restarted and your process died; your conversation is resumed. " +
		"Anything you had running is gone: a gate, a build, a test run, a background job, a command you were waiting on. " +
		"Re-run it, and treat any wait you had declared as over."
	if ask != nil {
		text += " Your review round (ask " + ask.Id + ") is still open: finish it and send your `done`."
	}
	return text
}

// resumeLost requests the resume of one agent lost at boot. nil: it is back (or needs none: it was resumed by hand,
// retired meanwhile or is gone); an error: a guard refused, and it is tried again.
func (d *Daemon) resumeLost(ctx context.Context, id string) error {
	var rings []*v1.Message
	err := d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !stopped(cur) || cur.State == v1.AgentState_AGENT_STATE_RETIRED || cur.PendingReopen != nil {
			return nil
		}
		if cur.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			model, effort, _ := target(cur)
			if err := d.reserveCheck(tx.Reader, cur.RunId, cur.Kind, model, effort, "resuming "+cur.Name); err != nil {
				return err
			}
		}
		if _, err := d.revive(tx, cur, reviveSpec{why: "machine restart"}); err != nil {
			return err
		}
		if err := tx.PutAgent(cur); err != nil {
			return err
		}
		var member *v1.Ask
		if asks, err := tx.OpenAsks(); err == nil {
			for _, ask := range asks {
				for _, m := range ask.Members {
					if m.AgentId == cur.Id && m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING {
						member = ask
					}
				}
			}
		}
		told, err := tellTx(tx, cur, restartInfo(cur, member))
		if err != nil {
			return err
		}
		rings = append(rings, told)
		if note, err := d.noteParentTx(tx, cur, "resumed", fmt.Sprintf("%s was lost in a machine restart and is resumed in its own conversation; "+
			"it was told what died with the machine (a gate or a job it had running).", cur.Name)); err != nil {
			return err
		} else if note != nil {
			rings = append(rings, note)
		}
		return nil
	})
	if err != nil {
		return err
	}
	d.deliverSoon(id)
	for _, m := range rings {
		if d.notify != nil {
			d.notify(m.ToId)
		}
	}
	return nil
}

// lostAfterRestart tells what the agent's exit alert would have: it was lost and could not be brought back. A pending
// member of an ask settles `lost`, and its asker is told when the round finishes.
func (d *Daemon) lostAfterRestart(ctx context.Context, id string, why error) {
	defer d.bootRelease(id) // after the write: until then the round's pass leaves the member to it
	reason := why.Error()
	var ce *connect.Error
	if errors.As(why, &ce) {
		reason = ce.Message()
	}
	var ring *v1.Message
	err := d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(id)
		if errors.Is(err, store.ErrNotFound) || err == nil && !stopped(cur) {
			return nil
		}
		if err != nil {
			return err
		}
		if !owesParent(cur) {
			return nil // a root has no parent to tell: it stays stopped, and `mesh resume` brings it back
		}
		text := fmt.Sprintf("%s lost its terminal in a machine restart and could not be resumed (%s). Resume it (`mesh resume %s`) or reassign the work.",
			cur.Name, reason, cur.Name)
		ring, err = d.alertParentTx(tx, cur, "exited", text, time.Time{})
		return err
	})
	if err != nil {
		d.log.Warn("telling a lost agent's parent", "agent", id, "err", err)
		return
	}
	if ring != nil && d.notify != nil {
		d.notify(ring.ToId)
	}
}
