package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

func TestRunStatusLineRecordsRateLimitsAndChainsUserCommand(t *testing.T) {
	state := t.TempDir()
	home := t.TempDir()
	userDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	userOut := filepath.Join(t.TempDir(), "seen.json")
	script := `cat > ` + userOut + ` && printf 'user-status-line\n'`
	settingsBytes, _ := json.Marshal(map[string]any{
		"statusLine": map[string]any{"type": "command", "command": script},
	})
	if err := os.WriteFile(filepath.Join(userDir, "settings.json"), settingsBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	payload := `{"session_id":"s1","rate_limits":{"five_hour":{"used_percentage":87.5,"resets_at":1800000000},"seven_day":{"utilization":42,"resets_at":"2026-12-01T00:00:00Z"}}}`
	var stdout bytes.Buffer
	fixedNow := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	env := map[string]string{"HOME": home}
	err := RunStatusLine(StatusLineOptions{
		StateDir: state,
		Stdin:    strings.NewReader(payload),
		Stdout:   &stdout,
		Env:      func(k string) string { return env[k] },
		Now:      func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("RunStatusLine: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "user-status-line" {
		t.Fatalf("stdout = %q, want user-status-line", got)
	}
	gotPayload, err := os.ReadFile(userOut)
	if err != nil || string(gotPayload) != payload {
		t.Fatalf("chained stdin = %q (%v), want %q", gotPayload, err, payload)
	}

	rawSnap, err := os.ReadFile(statedir.Paths{Dir: state}.ClaudeUsage())
	if err != nil {
		t.Fatalf("reading claude-usage.json: %v", err)
	}
	var snap ClaudeUsageSnapshot
	if err := json.Unmarshal(rawSnap, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Source != "statusline" {
		t.Errorf("source = %q, want statusline", snap.Source)
	}
	if w := snap.Windows["five_hour"]; w.Used != 87.5 || w.Resets == "" {
		t.Errorf("five_hour = %+v, want 87.5 with resets", w)
	}
	if w := snap.Windows["seven_day"]; w.Used != 42 || w.Resets != "2026-12-01T00:00:00Z" {
		t.Errorf("seven_day = %+v, want 42 @ 2026-12-01T00:00:00Z", w)
	}

	// Recursion guard skips chaining when AGENT_MESH_STATUSLINE=1.
	stdout.Reset()
	env["AGENT_MESH_STATUSLINE"] = "1"
	if err := RunStatusLine(StatusLineOptions{
		StateDir: state,
		Stdin:    strings.NewReader(payload),
		Stdout:   &stdout,
		Env:      func(k string) string { return env[k] },
		Now:      func() time.Time { return fixedNow },
	}); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("recursion guard leaked stdout: %q", stdout.String())
	}
}
