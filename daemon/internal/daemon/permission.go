package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Permission prompts (README, "Permission prompts"): a Claude agent's PermissionRequest hook asks the daemon, which
// tells the agent's parent (`[mesh permission <id>]`) and holds the hook until the parent decides, the wait runs
// out, or the agent goes on (it was answered at its terminal). No decision leaves the prompt to the person there.
// The pending prompts are in memory only: a hook blocked on a daemon that restarts prints nothing, and the
// prompt stays on the screen.

// permissionTag heads the message that tells a parent of a prompt.
const permissionTag = "[mesh permission "

// maxPermissionSummary is how many characters of a command or a path a parent is told.
const maxPermissionSummary = 300

type permDecision struct{ decision, reason string }

type pendingPermission struct {
	v1.PendingPermission
	ch chan permDecision // buffered 1: a decision, or the empty one when the prompt is over
}

// permTable is the prompts waiting for their agents' parents.
type permTable struct {
	mu      sync.Mutex
	pending map[string]*pendingPermission
}

func (t *permTable) add(p *pendingPermission) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		t.pending = map[string]*pendingPermission{}
	}
	t.pending[p.Id] = p
}

// take removes a prompt and says whether it was still pending.
func (t *permTable) take(id string) (*pendingPermission, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pending[id]
	delete(t.pending, id)
	return p, ok
}

func (t *permTable) get(id string) (*pendingPermission, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pending[id]
	return p, ok
}

func (t *permTable) list() []*v1.PendingPermission {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*v1.PendingPermission
	for _, p := range t.pending {
		out = append(out, proto.Clone(&p.PendingPermission).(*v1.PendingPermission))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.AsTime().Before(out[j].Since.AsTime()) })
	return out
}

// endFor ends the prompts of an agent (it went on: the prompt was answered at its terminal, or its turn is over)
// with no decision, so their hooks print nothing. With a tool, only the oldest prompt for it ends: the tool call
// that finished is one of possibly several in parallel, and it raised the oldest.
func (t *permTable) endFor(agentID, tool string) {
	t.mu.Lock()
	var ended []*pendingPermission
	var oldest *pendingPermission
	for _, p := range t.pending {
		if p.AgentId != agentID {
			continue
		}
		if tool == "" {
			ended = append(ended, p)
		} else if p.Tool == tool && (oldest == nil || p.Since.AsTime().Before(oldest.Since.AsTime())) {
			oldest = p
		}
	}
	if oldest != nil {
		ended = append(ended, oldest)
	}
	for _, p := range ended {
		delete(t.pending, p.Id)
	}
	t.mu.Unlock()
	for _, p := range ended {
		p.ch <- permDecision{}
	}
}

// permissionWait is how long a prompt is held for its parent: `permission.wait`, at most 540 s (the hook's own
// timeout is 600).
func (d *Daemon) permissionWait(r store.Reader) time.Duration {
	v, err := d.Setting(r, "permission.wait")
	f, ok := v.(float64)
	if err != nil || !ok {
		f = 300
	}
	if f > 540 {
		f = 540
	}
	return time.Duration(f * float64(time.Second))
}

// cutLine cuts text to n characters on one line.
func cutLine(s string, n int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n]) + "…"
	}
	return s
}

// RequestPermission is a hook asking for the answer to a prompt: it is held until the agent's parent decides.
func (d *Daemon) RequestPermission(ctx context.Context, req *connect.Request[v1.RequestPermissionRequest]) (*connect.Response[v1.RequestPermissionResponse], error) {
	actor := auth.ActorFrom(ctx)
	r := d.store.Read(ctx)
	a, err := d.getAgent(r, actor.Id)
	if err != nil {
		return nil, err
	}
	none := connect.NewResponse(&v1.RequestPermissionResponse{})
	if !owesParent(a) {
		return none, nil // a root's prompt is the user's
	}
	parent, err := r.GetAgent(a.ParentId)
	if err != nil || stopped(parent) {
		return none, nil // nobody to answer it
	}
	wait := d.permissionWait(r)
	if wait <= 0 {
		return none, nil
	}
	tool := cutLine(req.Msg.Tool, 80)
	summary := cutLine(req.Msg.Summary, maxPermissionSummary)
	p := &pendingPermission{ch: make(chan permDecision, 1)}
	p.Id, p.RunId, p.AgentId, p.AgentName, p.ParentId, p.Tool, p.Summary = ids.New("p", 8), a.RunId, a.Id, a.Name, parent.Id, tool, summary
	p.Since = tsOf(d.now())
	d.perms.add(p)
	defer d.perms.take(p.Id)

	text := fmt.Sprintf("%s%s] %s asks to use %s", permissionTag, p.Id, a.Name, tool)
	if summary != "" {
		text += ": " + summary
	}
	text += ". Answer it: `mesh permission --id " + p.Id + " --decision allow`, or `--decision deny --why \"<the reason, and what to do instead>\"`. " +
		"Allow only what the user said yes to; if unsure, deny and say so, or ask the user. Unanswered, it stays on the agent's screen."
	var told *v1.Message
	if err := d.store.Write(ctx, meshdActor, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(parent.Id)
		if err != nil || stopped(cur) {
			return err
		}
		told, err = tellTx(tx, cur, text)
		return err
	}); err != nil {
		return none, nil
	}
	if told != nil && d.notify != nil {
		d.notify(parent.Id)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case dec := <-p.ch:
		return connect.NewResponse(&v1.RequestPermissionResponse{Decision: dec.decision, Reason: dec.reason}), nil
	case <-timer.C:
	case <-ctx.Done():
	}
	// Out of time: take the prompt back. If it is already gone, a decision (or the end of the prompt) has been
	// taken and is on its way: wait for it, so the parent's answer is not lost.
	if _, pending := d.perms.take(p.Id); !pending {
		dec := <-p.ch
		return connect.NewResponse(&v1.RequestPermissionResponse{Decision: dec.decision, Reason: dec.reason}), nil
	}
	return none, nil
}

// DecidePermission answers a pending prompt: the agent's parent, or a device.
func (d *Daemon) DecidePermission(ctx context.Context, req *connect.Request[v1.DecidePermissionRequest]) (*connect.Response[v1.DecidePermissionResponse], error) {
	actor := auth.ActorFrom(ctx)
	m := req.Msg
	decision := strings.ToLower(strings.TrimSpace(m.Decision))
	if decision != "allow" && decision != "deny" {
		return nil, invalid("decide `allow` or `deny` (--decision)")
	}
	if decision == "deny" && strings.TrimSpace(m.Why) == "" {
		return nil, invalid("say why in a denial (--why) and what to do instead: the agent reads it")
	}
	p, ok := d.perms.get(m.Id)
	if !ok {
		return nil, notFound("no pending permission %q: it was answered at the agent's terminal, ran out, or the agent went on (`mesh permissions` lists the pending ones)", m.Id)
	}
	if IsAgent(actor.Class) && p.ParentId != actor.Id {
		return nil, denied("only %s's parent or a device may answer its permission prompt", p.AgentName)
	}
	if _, ok := d.perms.take(m.Id); !ok {
		return nil, notFound("no pending permission %q: it was just answered", m.Id)
	}
	p.ch <- permDecision{decision: decision, reason: strings.TrimSpace(m.Why)}
	out := proto.Clone(&p.PendingPermission).(*v1.PendingPermission)
	return connect.NewResponse(&v1.DecidePermissionResponse{Permission: out}), nil
}

// ListPermissions lists the prompts waiting for an answer: an agent's own children's, a device's every run's.
func (d *Daemon) ListPermissions(ctx context.Context, req *connect.Request[v1.ListPermissionsRequest]) (*connect.Response[v1.ListPermissionsResponse], error) {
	actor := auth.ActorFrom(ctx)
	out := &v1.ListPermissionsResponse{}
	for _, p := range d.perms.list() {
		switch {
		case IsAgent(actor.Class) && p.ParentId != actor.Id:
			continue
		case req.Msg.RunId != "" && p.RunId != req.Msg.RunId:
			continue
		}
		out.Permissions = append(out.Permissions, p)
	}
	return connect.NewResponse(out), nil
}

// permissionEnded ends the prompts of an agent that a hook shows went on: its turn ended or began again, its
// session ended, or the tool a prompt was for ran (a PostToolUse of that tool).
func (d *Daemon) permissionEnded(agentID, event string, payload []byte) {
	switch event {
	case "Stop", "StopFailure", "UserPromptSubmit", "SessionEnd":
		d.perms.endFor(agentID, "")
	case "PostToolUse":
		var p struct {
			Tool string `json:"tool_name"`
		}
		if json.Unmarshal(payload, &p) == nil && p.Tool != "" {
			d.perms.endFor(agentID, cutLine(p.Tool, 80))
		}
	}
}
