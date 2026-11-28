package daemon

// What an alert to a parent adds so it can judge without looking (bin/mesh's `evidence_line`, `wait_note` and
// `with_tail`): the worker's work (commits, pushed, PRs, a dirty worktree), a declared wait that has ended, and
// the last lines of its terminal.

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

const tailLines = 15 // lines of an agent's screen an alert quotes

// workTextCached is `Its work: …` from git and the PRs last read, with no call to GitHub: for a write that holds
// the database. "" for an agent with no branch of its own.
func (d *Daemon) workTextCached(r store.Reader, a *v1.Agent) string {
	if !ownsPRs(a) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	prs, _ := r.AgentPRs(a.Id)
	if t := evidenceText(workEvidence(ctx, a), prs, d.prs.read(a.Id)); t != "" {
		return "Its work: " + t + "."
	}
	return ""
}

// screenPrinted is what a terminal's process printed last, on one line (its newest screen lines, at most
// exitPrintedCut characters): "" when nothing can be read. It says why a CLI that exited at once did.
func (d *Daemon) screenPrinted(ctx context.Context, terminalID string) string {
	if terminalID == "" {
		return ""
	}
	raw, err := d.readScreen(ctx, terminalID)
	if err != nil && len(raw) == 0 {
		return ""
	}
	var lines []string
	for _, l := range opencode.ScreenLines(raw) {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return cutLine(strings.Join(lines, " "), exitPrintedCut)
}

const exitPrintedCut = 200

// screenTail is the last lines of an agent's terminal, as text a parent can read: "" when it can't be read.
func (d *Daemon) screenTail(ctx context.Context, a *v1.Agent) string {
	if a.TerminalId == "" {
		return ""
	}
	raw, err := d.readScreen(ctx, a.TerminalId)
	if err != nil && len(raw) == 0 {
		return ""
	}
	var lines []string
	for _, l := range opencode.ScreenLines(raw) {
		if l = strings.TrimRight(l, " \t"); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	if len(lines) == 0 {
		return ""
	}
	return "\nLast lines of its terminal:\n  " + strings.Join(lines, "\n  ")
}

// afterwards is what an alert about a stuck agent adds: its work, then the tail of its terminal.
func (d *Daemon) afterwards(ctx context.Context, a *v1.Agent) string {
	out := ""
	if t := d.workText(ctx, a); t != "" {
		out = " " + t
	}
	return out + d.screenTail(ctx, a)
}

// declaredWait is when the agent last declared a wait since its last task or answer, or zero.
func declaredWait(r store.Reader, a *v1.Agent) time.Time {
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return time.Time{}
	}
	var wait time.Time
	for _, m := range msgs {
		switch {
		case m.ToId == a.Id && (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER):
			wait = time.Time{}
		case m.From.GetId() == a.Id && m.ToId == a.ParentId && m.Type == v1.MessageType_MESSAGE_TYPE_WAIT:
			wait = m.CreatedAt.AsTime()
		}
	}
	return wait
}

// silentExtras is what the silent-agent alert adds to its text: that a declared wait had ended, its work, and
// its terminal.
func (d *Daemon) silentExtras(ctx context.Context, a *v1.Agent) string {
	out := ""
	if w := declaredWait(d.store.Read(ctx), a); !w.IsZero() {
		out += fmt.Sprintf(" It had declared a wait %s ago, which is over.", ago(d.now().Sub(w)))
	}
	return out + d.afterwards(ctx, a)
}
