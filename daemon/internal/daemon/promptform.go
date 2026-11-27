package daemon

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
)

// The two tools whose permission prompt is a form for the user, not a yes or no: Claude's AskUserQuestion (the
// questions are the tool's input, the answers go back as `answers` in an updated input) and ExitPlanMode (the plan
// is the input, the approval may set the permission mode). The hook sends their whole input; the daemon reads it
// into the prompt, so an app can draw the form and any client can answer it.

// maxFormInput is the largest tool input read into a prompt; a bigger one is left to the terminal.
const maxFormInput = 256 << 10

// promptKind is the kind of prompt a tool makes.
func promptKind(tool string) v1.PermissionKind {
	switch tool {
	case claude.ToolAskUserQuestion:
		return v1.PermissionKind_PERMISSION_KIND_QUESTION
	case claude.ToolExitPlanMode:
		return v1.PermissionKind_PERMISSION_KIND_PLAN
	}
	return v1.PermissionKind_PERMISSION_KIND_TOOL
}

// readForm fills a question or plan prompt from the tool's input, and says whether it could: a malformed input (a
// CLI version whose shape changed) is not forwarded, so the prompt stays with the person at the terminal.
func readForm(p *v1.PendingPermission, input string) bool {
	if len(input) > maxFormInput {
		return false
	}
	switch p.Kind {
	case v1.PermissionKind_PERMISSION_KIND_QUESTION:
		p.Questions = claude.ParseQuestions([]byte(input))
		return len(p.Questions) > 0
	case v1.PermissionKind_PERMISSION_KIND_PLAN:
		var in struct {
			Plan     string `json:"plan"`
			PlanFile string `json:"planFilePath"`
		}
		if json.Unmarshal([]byte(input), &in) != nil || strings.TrimSpace(in.Plan) == "" {
			return false
		}
		p.Plan, p.PlanFile = in.Plan, in.PlanFile
		return true
	}
	return true
}

// buildAnswers checks a user's answers against the questions of a prompt and makes what the CLI takes back: one
// string for each question, by its text. A single choice is one label or the text written under "Other"; a
// multi-select is its labels, then the "Other" text, joined with ", " (what Claude's own dialog gives).
func buildAnswers(questions []*v1.PromptQuestion, in []*v1.PromptAnswer) (map[string]string, error) {
	byQ := map[string]*v1.PromptAnswer{}
	for _, a := range in {
		if _, dup := byQ[a.Question]; dup {
			return nil, invalid("question %q is answered twice", a.Question)
		}
		byQ[a.Question] = a
	}
	out := map[string]string{}
	for _, q := range questions {
		a := byQ[q.Question]
		delete(byQ, q.Question)
		if a == nil {
			return nil, invalid("answer every question: %q has none", q.Question)
		}
		labels := map[string]bool{}
		for _, o := range q.Options {
			labels[o.Label] = true
		}
		var parts []string
		for _, l := range a.Labels {
			if !labels[l] {
				return nil, invalid("%q is not an option of %q", l, q.Question)
			}
			parts = append(parts, l)
		}
		other := strings.TrimSpace(a.Other)
		if other != "" {
			parts = append(parts, other)
		}
		switch {
		case len(parts) == 0:
			return nil, invalid("answer every question: %q has none", q.Question)
		case !q.MultiSelect && len(parts) > 1:
			return nil, invalid("%q takes one answer: a label or the \"Other\" text, not both or several", q.Question)
		}
		out[q.Question] = strings.Join(parts, ", ")
	}
	for text := range byQ {
		return nil, invalid("%s", fmt.Sprintf("%q is not a question of this prompt", text))
	}
	return out, nil
}

// planMode checks the mode an approved plan goes on in: empty, `default` or `acceptEdits`.
func planMode(mode string) (string, error) {
	switch mode {
	case "", "default", "acceptEdits":
		return mode, nil
	}
	return "", invalid("a plan goes on in `default` or `acceptEdits`, not %q", mode)
}
