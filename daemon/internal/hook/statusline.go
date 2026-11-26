package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

// StatusLineOptions configures `mesh statusline --state <dir>`.
type StatusLineOptions struct {
	StateDir string
	Stdin    io.Reader
	Stdout   io.Writer
	Env      func(string) string
	Now      func() time.Time
	// Timeout bounds how long a chained user statusLine command may run.
	Timeout time.Duration
}

// ClaudeUsageSnapshot is written to <state>/claude-usage.json by `mesh statusline` and read by meshd.
type ClaudeUsageSnapshot struct {
	Windows map[string]ClaudeUsageWindow `json:"windows"`
	Fetched float64                      `json:"fetched"`
	Source  string                       `json:"source,omitempty"`
}

// ClaudeUsageWindow is one recorded Claude quota window.
type ClaudeUsageWindow struct {
	Used   float64 `json:"used"`
	Resets string  `json:"resets,omitempty"`
}

var claudeWindowKeys = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}

// RunStatusLine records Claude Code's `rate_limits` from stdin into <state>/claude-usage.json and, if the
// user's own global ~/.claude/settings.json defines a statusLine command, runs it with the same stdin and
// copies its stdout so the user's status line is preserved. Never errors out to the caller.
func RunStatusLine(o StatusLineOptions) error {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Env == nil {
		o.Env = os.Getenv
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	raw, _ := io.ReadAll(io.LimitReader(o.Stdin, MaxPayload))
	if o.StateDir != "" && len(raw) > 0 {
		if windows := parseStatusLineRateLimits(raw); len(windows) > 0 {
			snap := ClaudeUsageSnapshot{
				Windows: windows,
				Fetched: float64(o.Now().UnixNano()) / 1e9,
				Source:  "statusline",
			}
			if b, err := json.Marshal(snap); err == nil {
				p := statedir.Paths{Dir: o.StateDir}
				_ = os.MkdirAll(o.StateDir, 0o700)
				if err := fsutil.WriteAtomic(p.ClaudeUsage(), b, 0o600); err == nil {
					_ = os.Remove(p.ClaudeUsageFailed())
				}
			}
		}
	}
	if o.Env("AGENT_MESH_STATUSLINE") == "1" {
		return nil
	}
	if cmdLine := userStatusLineCommand(o.Env); cmdLine != "" {
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdLine)
		cmd.Stdin = bytes.NewReader(raw)
		cmd.Env = append(os.Environ(), "AGENT_MESH_STATUSLINE=1")
		if out, err := cmd.Output(); err == nil && o.Stdout != nil {
			_, _ = o.Stdout.Write(out)
		}
	}
	return nil
}

func parseStatusLineRateLimits(raw []byte) map[string]ClaudeUsageWindow {
	var top struct {
		RateLimits map[string]json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &top) != nil || len(top.RateLimits) == 0 {
		return nil
	}
	out := map[string]ClaudeUsageWindow{}
	for _, key := range claudeWindowKeys {
		wraw, ok := top.RateLimits[key]
		if !ok || len(bytes.TrimSpace(wraw)) == 0 || string(bytes.TrimSpace(wraw)) == "null" {
			continue
		}
		var w struct {
			UsedPercentage *float64 `json:"used_percentage"`
			Utilization    *float64 `json:"utilization"`
			ResetsAt       any      `json:"resets_at"`
		}
		if json.Unmarshal(wraw, &w) != nil {
			continue
		}
		var used float64
		switch {
		case w.UsedPercentage != nil:
			used = *w.UsedPercentage
		case w.Utilization != nil:
			used = *w.Utilization
		default:
			continue
		}
		out[key] = ClaudeUsageWindow{
			Used:   used,
			Resets: formatStatusLineReset(w.ResetsAt),
		}
	}
	return out
}

func formatStatusLineReset(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x <= 0 {
			return ""
		}
		sec := int64(x)
		if x > 1e12 {
			sec = int64(x / 1000)
		}
		return time.Unix(sec, 0).UTC().Format(time.RFC3339)
	default:
		return ""
	}
}

func userStatusLineCommand(getenv func(string) string) string {
	dir := getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		StatusLine *struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if json.Unmarshal(b, &cfg) != nil || cfg.StatusLine == nil {
		return ""
	}
	if cfg.StatusLine.Type != "" && cfg.StatusLine.Type != "command" {
		return ""
	}
	return strings.TrimSpace(cfg.StatusLine.Command)
}
