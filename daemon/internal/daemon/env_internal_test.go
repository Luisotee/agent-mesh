package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1/termv1connect"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

func TestLookPathAndTheAgentEnvAllowlist(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "notexec"), []byte("x"), 0o644)
	if p, ok := lookPath("claude", "/nonexistent:"+dir); !ok || p != bin {
		t.Errorf("lookPath: %q, %v", p, ok)
	}
	if _, ok := lookPath("notexec", dir); ok {
		t.Error("a file without the execute bit was found")
	}
	if _, ok := lookPath("claude", ""); ok {
		t.Error("found on an empty PATH")
	}
	have := map[string]bool{}
	for _, k := range inheritedEnv {
		have[k] = true
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "HTTPS_PROXY", "NO_PROXY",
		"CLAUDE_CODE_USE_BEDROCK", "AWS_PROFILE", "CLAUDE_CODE_USE_VERTEX", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if !have[k] {
			t.Errorf("%s is not inherited by agents", k)
		}
	}
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "AGENT_MESH_TOKEN", "MESH_RUN", "ANTIGRAVITY_AGENT"} {
		if have[k] {
			t.Errorf("%s must never be inherited", k)
		}
	}
}

// stubTerm answers terminal writes from a script: one error per Enter write, in order.
type stubTerm struct {
	termv1connect.TermHostServiceClient
	enterErrs []error
	writes    []string
}

func (s *stubTerm) WriteTerminal(_ context.Context, r *connect.Request[termv1.WriteTerminalRequest]) (*connect.Response[termv1.WriteTerminalResponse], error) {
	data := string(r.Msg.Data)
	s.writes = append(s.writes, data)
	if data == "\r" && len(s.enterErrs) > 0 {
		err := s.enterErrs[0]
		s.enterErrs = s.enterErrs[1:]
		return nil, err
	}
	return connect.NewResponse(&termv1.WriteTerminalResponse{}), nil
}

func TestTypeLineRetriesEnterOnlyWhereTheWriteDidNotHappen(t *testing.T) {
	down := connect.NewError(connect.CodeUnavailable, errors.New("down"))
	cases := []struct {
		name       string
		errs       []error
		wantErr    bool
		wantWrites string
	}{
		{"a clean write", nil, false, "line,\r"},
		{"unavailable once, then through", []error{down}, false, "line,\r,\r"},
		{"unavailable every time: the text is taken back out", []error{down, down, down}, true, "line,\r,\r,\r,\x15"},
		{"unknown: stop, no Ctrl-U", []error{connect.NewError(connect.CodeUnknown, errors.New("?"))}, true, "line,\r"},
		{"an internal error: stop, no Ctrl-U", []error{connect.NewError(connect.CodeInternal, errors.New("?"))}, true, "line,\r"},
	}
	for _, c := range cases {
		stub := &stubTerm{enterErrs: c.errs}
		d := &Daemon{rt: &runtime{cfg: AgentConfig{DeliveryDelay: time.Millisecond}, term: &termclient.Client{API: stub}}}
		err := d.typeLine(context.Background(), "t1", "line")
		if (err != nil) != c.wantErr || strings.Join(stub.writes, ",") != c.wantWrites {
			t.Errorf("%s: err %v, writes %q, want error %v and %q", c.name, err, strings.Join(stub.writes, ","), c.wantErr, c.wantWrites)
		}
	}
}

func TestTypePromptPastesWhatATypedKeyWouldMangleAndTakesAFailedPasteBack(t *testing.T) {
	reg, err := adapter.NewRegistry(context.Background(), claude.New(), agy.New())
	if err != nil {
		t.Fatal(err)
	}
	down := connect.NewError(connect.CodeUnavailable, errors.New("down"))
	cases := []struct {
		name, kind, text string
		errs             []error
		wantErr          bool
		wantWrites       string
	}{
		{"one plain line is typed", "claude", "a b", nil, false, "a b,\r"},
		{"a tab is pasted, not typed as a Tab key", "claude", "a\tb", nil, false, "\x1b[200~a\tb\x1b[201~,\r"},
		{"lines are pasted", "claude", "a\nb", nil, false, "\x1b[200~a\nb\x1b[201~,\r"},
		{"a CLI without paste gets spaces for a tab", "agy", "a\tb", nil, false, "a b,\r"},
		{"and one space per run of white space for lines", "agy", "a\n\tb", nil, false, "a b,\r"},
		{"a paste whose Enter never went is taken back out", "claude", "a\nb", []error{down, down, down}, true, "\x1b[200~a\nb\x1b[201~,\r,\r,\r,\x15"},
		{"an unknown Enter failure sends nothing more", "claude", "a\nb", []error{connect.NewError(connect.CodeUnknown, errors.New("?"))}, true, "\x1b[200~a\nb\x1b[201~,\r"},
	}
	for _, c := range cases {
		stub := &stubTerm{enterErrs: c.errs}
		d := &Daemon{rt: &runtime{adapters: reg, cfg: AgentConfig{DeliveryDelay: time.Millisecond}, term: &termclient.Client{API: stub}}}
		err := d.typePrompt(context.Background(), &v1.Agent{Kind: c.kind, TerminalId: "t1"}, c.text)
		if (err != nil) != c.wantErr || strings.Join(stub.writes, ",") != c.wantWrites {
			t.Errorf("%s: err %v, writes %q, want error %v and %q", c.name, err, strings.Join(stub.writes, ","), c.wantErr, c.wantWrites)
		}
	}
}
