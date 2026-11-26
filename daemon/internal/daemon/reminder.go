package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The SessionStart reminder (docs/design.md, "The Stop hook"): a Claude session that starts again (a compaction, a
// /clear, a resume) may have lost its context, and Claude adds what a SessionStart hook prints to it. The hook
// prints ReportHookResponse.context, which is this text. Its wording must not look like a doorbell. It carries no
// wall-clock time, so two renders of the same state match, and stays within a hook's output cap.

// reminderSources are the SessionStart sources that get a reminder.
var reminderSources = map[string]bool{"compact": true, "clear": true, "resume": true}

// Task text the reminder carries: a worker can't read a message again (only its inbox's unread ones), so the
// reminder holds what it needs of the current task and its newest answer.
const (
	reminderTask   = 2000
	reminderAnswer = 1000
)

// reminderSource is the source of a SessionStart among decoded events, or "".
func reminderSource(a *v1.Agent, evs []*adapterv1.AgentEvent) string {
	if a.Kind != claude.Kind {
		return ""
	}
	for _, ev := range evs {
		if ss := ev.GetSessionStarted(); ss != nil && reminderSources[ss.Source] {
			return ss.Source
		}
	}
	return ""
}

// currentTask is the newest task sent to an agent, and the newest answer after it (if any).
func currentTask(r store.Reader, a *v1.Agent) (task, answer *v1.Message, err error) {
	msgs, err := r.Inbox(a.Id, true)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range msgs {
		switch m.Type {
		case v1.MessageType_MESSAGE_TYPE_TASK:
			task, answer = m, nil
		case v1.MessageType_MESSAGE_TYPE_ANSWER:
			if task != nil {
				answer = m
			}
		}
	}
	return task, answer, nil
}

// workerPhase says where a worker is in its task, by its newest report and its phase (bin/mesh's worker_phase).
func workerPhase(a *v1.Agent, report *v1.Message, task string) string {
	if report != nil {
		switch report.Type {
		case v1.MessageType_MESSAGE_TYPE_PLAN:
			return "your plan is waiting for its answer: change nothing until it says approved"
		case v1.MessageType_MESSAGE_TYPE_QUESTION:
			return "your question is waiting for its answer"
		default:
			return fmt.Sprintf("you reported `%s` on it: wait for a new task or answer", strings.ToLower(strings.TrimPrefix(report.Type.String(), "MESSAGE_TYPE_")))
		}
	}
	task = strings.TrimSpace(task)
	switch {
	case isReadOnlyTask(task):
		return "a read-only task: change nothing, and report one `done` with the answer"
	case isDirectTask(task):
		return "a direct task (no plan review by your parent): make the change, verify it, open the PR, run the `self-review` skill on it, report `done`"
	case a.Phase == v1.AgentPhase_AGENT_PHASE_PLANNING:
		s := "planning: research, write the plan, run the `check-plan` skill on it, then send it as a `plan`, and change nothing until it is approved"
		if a.DenyEdits {
			s += " (until then mesh denies edits to your worktree and `git push`: a denial is expected, not something to work around)"
		}
		return s
	case a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING:
		return "executing your approved plan: finish, verify, open the PR, run the `self-review` skill on it, then report `done`"
	}
	return "working on it: report `done` or `failed` with `mesh send parent` when it's finished (stuck: send a `question`)"
}

// reminderText is what a Claude session that started again (source: compact, clear or resume) is told. A root's
// is silent while its reset is typing: the prompt typed next says `mesh brief`.
func (d *Daemon) reminderText(ctx context.Context, a *v1.Agent, source string) string {
	r := d.store.Read(ctx)
	dir := d.paths.RunDir(a.RunId)
	if a.Role == v1.AgentRole_AGENT_ROLE_ROOT && d.isSetupRun(r, a.RunId) {
		line := fmt.Sprintf("agent-mesh reminder (%s): you are %s, the setup agent of run %s (no parent to report to). Your earlier context may be gone.", source, a.Name, a.RunId)
		if share := d.rt.cfg.Share; filepath.IsAbs(share) {
			line += " Read " + filepath.Join(share, "protocol", setupFile) + " again and carry on with the user."
		}
		return line
	}
	if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
		if a.PendingReset != nil && a.PendingReset.TypedAt != nil {
			return ""
		}
		return fmt.Sprintf("agent-mesh reminder (%s): you are %s, the root of run %s (no parent to report to). Your earlier context may be gone: "+
			"run `mesh brief` now and act by the root card it prints, not from memory. Run dir: %s.", source, a.Name, a.RunId, dir)
	}
	parent := a.ParentId
	if p, err := d.getAgent(r, a.ParentId); err == nil {
		parent = p.Name
	}
	line := fmt.Sprintf("agent-mesh reminder (%s): you are %s, a %s in run %s, and your parent is %s. Run dir: %s.", source, a.Name,
		strings.ToLower(strings.TrimPrefix(a.Role.String(), "AGENT_ROLE_")), a.RunId, parent, dir)
	if share := d.rt.cfg.Share; filepath.IsAbs(share) {
		line += " Protocol: " + filepath.Join(share, "protocol", "PROTOCOL.md") + "."
	}
	lines := []string{line}
	task, answer, err := currentTask(r, a)
	if err != nil || task == nil {
		lines = append(lines, "No task yet.")
	} else {
		report, _ := lastReport(r, a)
		lines = append(lines, fmt.Sprintf("Current task: %s", taskGist(task.Body)), capReminder(task.Body, reminderTask))
		if answer != nil {
			lines = append(lines, "Latest answer from "+answer.From.GetName()+":", capReminder(answer.Body, reminderAnswer))
		}
		lines = append(lines, "Phase: "+workerPhase(a, report, task.Body)+".")
	}
	lines = append(lines, "Report with `mesh send parent …`. Run `mesh inbox` now.")
	return strings.Join(lines, "\n")
}

// capReminder is a message body cut to limit characters, saying so.
func capReminder(body string, limit int) string {
	body = strings.TrimSpace(body)
	if r := []rune(body); len(r) > limit {
		return string(r[:limit]) + "\n… (cut: the rest is in the task message your parent sent)"
	}
	return body
}

// HookOutputMax is what Claude Code keeps of a hook's output: past 10,000 characters the session gets a file path
// and a 2,000-character preview, and is never told to read the file.
const HookOutputMax = 10_000

// hookText is text within a hook's output cap (less the newline the hook adds): cut from the middle at a line end,
// keeping the last line (the reminder's commands) when it fits.
func hookText(text string) string {
	limit := HookOutputMax - 1
	if len(text) <= limit {
		return text
	}
	more := "\n… (cut to fit a hook's output)\n"
	tail := ""
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		tail = text[i+1:]
	}
	if tail == text || len(tail)+len(more) > limit/2 {
		tail = ""
	}
	room := limit - len(more) - len(tail)
	if room < 0 {
		room = 0
	}
	cut := strings.LastIndex(text[:utf8Floor(text, room)], "\n")
	if cut <= 0 {
		cut = utf8Floor(text, room)
	}
	out := text[:cut] + more + tail
	if len(out) > limit {
		out = out[:utf8Floor(out, limit)]
	}
	return out
}

// utf8Floor is the largest n' <= n at which s can be cut without splitting a character.
func utf8Floor(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}
