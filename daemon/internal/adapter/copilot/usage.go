package copilot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Call is one model call Copilot recorded in a session's events.jsonl (a `session.usage_record` event).
type Call struct {
	// ID is the event's id, the same across reads.
	ID    string
	Time  time.Time
	Model string
	// Input is the prompt's fresh tokens: Copilot's `inputTokens` counts the cache reads and writes in it, so they
	// are taken out here and given in Read and Write.
	Input, Read, Write int64
	// Output is the tokens the model wrote (Copilot's `outputTokens`; its reasoning tokens are part of it).
	Output int64
}

type usageRecord struct {
	Model            string `json:"model"`
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
}

// SessionCalls reads every model call of a session from its events.jsonl, oldest first (nil when there is no file).
func SessionCalls(env map[string]string, sessionID string) []Call {
	if sessionID == "" {
		return nil
	}
	f, err := os.Open(EventsPath(env, sessionID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var calls []Call
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		// Only complete lines count: the last one may be half written.
		if len(line) > 0 && line[len(line)-1] == '\n' && bytes.Contains(line, []byte(`"session.usage_record"`)) {
			if c, ok := usageCall(line); ok {
				calls = append(calls, c)
			}
		}
		if err != nil {
			break
		}
	}
	return calls
}

func usageCall(line []byte) (Call, bool) {
	var ev Event
	if json.Unmarshal(line, &ev) != nil || ev.Type != "session.usage_record" {
		return Call{}, false
	}
	var d struct {
		Usage usageRecord `json:"usage"`
	}
	if json.Unmarshal(ev.Data, &d) != nil {
		return Call{}, false
	}
	u := d.Usage
	at := ev.Time()
	if at.IsZero() || u.InputTokens+u.OutputTokens == 0 {
		return Call{}, false
	}
	return Call{ID: ev.ID, Time: at, Model: u.Model, Input: max(0, u.InputTokens-u.CacheReadTokens-u.CacheWriteTokens),
		Read: u.CacheReadTokens, Write: u.CacheWriteTokens, Output: u.OutputTokens}, true
}

// RecentSessions lists the sessions whose events.jsonl was written at or after since (all when zero).
func RecentSessions(env map[string]string, since time.Time) []string {
	entries, err := os.ReadDir(filepath.Join(Home(env), "session-state"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(Home(env), "session-state", e.Name(), "events.jsonl"))
		if err != nil || (!since.IsZero() && info.ModTime().Before(since)) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}
