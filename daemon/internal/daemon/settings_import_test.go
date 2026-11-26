package daemon_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

const ownerPolicy = `{
  "repos_dir": ["~/projects"],
  "compact": {"claude": 250000},
  "root_reset": {"relaunch": false},
  "review": {"reviewers": {"small": [["opencode","agy","claude"]], "large": [["agy","claude"]]}},
  "claude": {"mcp_keep": ["context7"]},
  "hang_after": 60,
  "nope": 1
}`

func writePolicy(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent-mesh.json")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func withPolicy(path string) func(*daemon.Config) {
	return func(c *daemon.Config) { c.PolicyFile = path }
}

func raw(s *v1.Setting) string { return s.Value.String() }

func TestMeshdImportsThePolicyFileOnItsFirstStartOnly(t *testing.T) {
	file := writePolicy(t, ownerPolicy)
	state := filepath.Join(t.TempDir(), "state")
	h := startWith(t, state, withPolicy(file))
	if s := setting(t, h, "compact.claude"); !s.IsSet || s.Value.GetNumberValue() != 250000 {
		t.Errorf("compact.claude %v", s)
	}
	if s := setting(t, h, "root_reset.relaunch"); !s.IsSet || s.Value.GetBoolValue() {
		t.Errorf("root_reset.relaunch %v", s)
	}
	if s := setting(t, h, "repos.dir"); !s.IsSet || s.Value.GetListValue().GetValues()[0].GetStringValue() != "~/projects" {
		t.Errorf("repos.dir %v", s)
	}
	if s := setting(t, h, "review.reviewers.large"); !s.IsSet || strings.Count(raw(s), "agy") != 1 {
		t.Errorf("review.reviewers.large %v", s)
	}
	if setting(t, h, "quota_stop").IsSet {
		t.Error("a leaf the file doesn't name is set")
	}
	evs := settingEvents(t, h)
	if len(evs) != 6 {
		t.Errorf("%d SettingChanged events, want one per leaf (6)", len(evs))
	}
	h.stop()

	// The user changes a setting in the app, then edits the file: a restart keeps the first and ignores the second.
	h = startWith(t, state, withPolicy(file))
	if _, err := h.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{
		Meta: meta("m1"), Key: "compact.claude", Value: val(t, 111000.0)})); err != nil {
		t.Fatal(err)
	}
	h.stop()
	os.WriteFile(file, []byte(`{"compact": {"claude": 999, "agy": 5}}`), 0o644)
	h = startWith(t, state, withPolicy(file))
	if s := setting(t, h, "compact.claude"); s.Value.GetNumberValue() != 111000 {
		t.Errorf("a restart re-imported over a setting: %v", s)
	}
	if setting(t, h, "compact.agy").IsSet {
		t.Error("an edited file was imported again at a restart")
	}
}

func TestMeshdWithNoPolicyFileOrABrokenOneStartsAnyway(t *testing.T) {
	h := startWith(t, "", withPolicy(filepath.Join(t.TempDir(), "absent.json")))
	if setting(t, h, "compact.claude").IsSet {
		t.Error("set from nothing")
	}
	h = startWith(t, "", withPolicy(writePolicy(t, `{not json`)))
	if setting(t, h, "compact.claude").IsSet {
		t.Error("set from a broken file")
	}
}

// syncBuffer is a log sink the daemon's goroutines write while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestMeshdLogsAChangedPolicyFile(t *testing.T) {
	file := writePolicy(t, `{"compact": {"agy": 5}}`)
	state := filepath.Join(t.TempDir(), "state")
	startWith(t, state, withPolicy(file)).stop()
	os.WriteFile(file, []byte(`{"compact": {"agy": 6}}`), 0o644)
	logs := &syncBuffer{}
	startWith(t, state, func(c *daemon.Config) {
		c.PolicyFile = file
		c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}).stop()
	if !strings.Contains(logs.String(), "policy file changed since its import") {
		t.Errorf("log: %s", logs.String())
	}
	// Applying it by hand is its import: the next start says nothing.
	h := startWith(t, state, withPolicy(file))
	if _, err := h.device.Mesh.ImportSettings(context.Background(), connect.NewRequest(&v1.ImportSettingsRequest{Meta: meta("apply")})); err != nil {
		t.Fatal(err)
	}
	h.stop()
	logs = &syncBuffer{}
	startWith(t, state, func(c *daemon.Config) {
		c.PolicyFile = file
		c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}).stop()
	if strings.Contains(logs.String(), "changed since") {
		t.Errorf("a file applied by hand still warns: %s", logs.String())
	}
}

func TestImportSettingsAppliesOnDemandAndKeepsWhatIsSet(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	call := func(mid string, req *v1.ImportSettingsRequest) *v1.ImportSettingsResponse {
		t.Helper()
		req.Meta = meta(mid)
		res, err := h.device.Mesh.ImportSettings(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	if _, err := h.device.Mesh.ImportSettings(ctx, connect.NewRequest(&v1.ImportSettingsRequest{Meta: meta("none")})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no file given and none configured: %v", err)
	}
	file := writePolicy(t, ownerPolicy+"\n")
	if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta("pre"), Key: "compact.claude", Value: val(t, 1.0)})); err != nil {
		t.Fatal(err)
	}
	dry := call("dry", &v1.ImportSettingsRequest{From: file, DryRun: true})
	if !dry.DryRun || len(dry.Imported) != 5 || setting(t, h, "root_reset.relaunch").IsSet {
		t.Errorf("dry run: %v", dry)
	}
	got := call("one", &v1.ImportSettingsRequest{From: file})
	if len(got.Imported) != 5 || got.File != file {
		t.Errorf("imported %v", got.Imported)
	}
	skipped := strings.Join(got.Skipped, "\n")
	for _, want := range []string{"compact.claude: already set", "hang_after: not used by the daemon", "nope: no such setting"} {
		if !strings.Contains(skipped, want) {
			t.Errorf("skipped lacks %q: %v", want, got.Skipped)
		}
	}
	if setting(t, h, "compact.claude").Value.GetNumberValue() != 1 {
		t.Error("a set leaf was replaced")
	}
	// A retry with the same mutation id replays the first answer, not a fresh (empty) one.
	if replay := call("one", &v1.ImportSettingsRequest{From: file}); len(replay.Imported) != 5 {
		t.Errorf("replay: %v", replay.Imported)
	}
	if again := call("two", &v1.ImportSettingsRequest{From: file}); len(again.Imported) != 0 {
		t.Errorf("a second import changed %v", again.Imported)
	}
	over := call("three", &v1.ImportSettingsRequest{From: file, Overwrite: true})
	if len(over.Imported) != 1 || over.Imported[0].Key != "compact.claude" || setting(t, h, "compact.claude").Value.GetNumberValue() != 250000 {
		t.Errorf("overwrite: %v", over.Imported)
	}
	if _, err := h.device.Mesh.ImportSettings(ctx, connect.NewRequest(&v1.ImportSettingsRequest{Meta: meta("bad"), From: writePolicy(t, `[1]`)})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("not an object: %v", err)
	}
	if _, err := h.device.Mesh.ImportSettings(ctx, connect.NewRequest(&v1.ImportSettingsRequest{Meta: meta("gone"), From: "/nonexistent/x.json"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("missing file: %v", err)
	}
}

func TestImportSettingsMapsCopilotModelAndTheRenamedLeaves(t *testing.T) {
	h := start(t, "")
	file := writePolicy(t, `{"copilot_model": "gpt-5", "peers_dir": "~/r"}`)
	res, err := h.device.Mesh.ImportSettings(context.Background(), connect.NewRequest(&v1.ImportSettingsRequest{Meta: meta("c"), From: file}))
	if err != nil || len(res.Msg.Imported) != 4 {
		t.Fatalf("%v %v", res, err)
	}
	if s := setting(t, h, "tiers.copilot.mid.model"); s.Value.GetListValue().GetValues()[0].GetStringValue() != "gpt-5" {
		t.Errorf("%v", s)
	}
	if s := setting(t, h, "peers.dir"); s.Value.GetStringValue() != "~/r" {
		t.Errorf("%v", s)
	}
}
