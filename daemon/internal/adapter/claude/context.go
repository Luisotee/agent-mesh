package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// Context size (docs/design.md §12, §15): Claude Code appends every message to a per-session JSONL file, and each
// assistant reply records the prompt it was given (`usage`). The newest reply's prompt is what the next call
// re-reads, until a `compact_boundary` line says the session was compacted.

const (
	// TTLLookback is how many replies past the newest are read for one that wrote to the cache.
	TTLLookback = 50
	// DefaultCacheTTL is the cache's lifetime when no reply says (a subscription within its plan: 1 h).
	DefaultCacheTTL = time.Hour
	// shortCache is a one-off compaction turn's cache: 5 minutes is all it needs.
	shortCache = "5m"
)

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// TranscriptPath is where Claude keeps a session run in cwd: `<config>/projects/<cwd with [^A-Za-z0-9] as ->/<id>.jsonl`.
func TranscriptPath(env map[string]string, cwd, session string) string {
	home := configHome(env)
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".claude")
		}
	}
	if home == "" || cwd == "" || session == "" {
		return ""
	}
	return filepath.Join(home, "projects", nonAlnum.ReplaceAllString(cwd, "-"), session+".jsonl")
}

type usageLine struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	IsSidechain bool   `json:"isSidechain"`
	// CompactMetadata is on a compact_boundary: Claude Code 2.1.332 records the conversation's size before
	// (preTokens) and after (postTokens: the messages kept, without the system prompt and tools, which the next
	// reply adds) the compaction.
	CompactMetadata struct {
		PostTokens int64 `json:"postTokens"`
	} `json:"compactMetadata"`
	Message struct {
		Usage struct {
			Input         int64 `json:"input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			Writes        struct {
				Hour int64 `json:"ephemeral_1h_input_tokens"`
				Five int64 `json:"ephemeral_5m_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
	hasUsage bool
}

func parseUsageLine(line []byte) (usageLine, bool) {
	var l usageLine
	if !bytes.Contains(line, []byte(`"compact_boundary"`)) && !bytes.Contains(line, []byte(`"usage"`)) {
		return l, false
	}
	if json.Unmarshal(line, &l) != nil {
		return l, false
	}
	l.hasUsage = bytes.Contains(line, []byte(`"usage"`))
	return l, true
}

// lineTokens is a line's context size: a compaction's postTokens (0 when the line has none), the main conversation's assistant reply's prompt, else
// false. A subagent's reply (isSidechain) is another conversation's prompt, and a reply with no prompt tokens at
// all (Claude Code's synthetic replies for an API error or a limit) says nothing about the size: both are skipped,
// so the scan goes on to the reply before.
func lineTokens(line []byte) (int64, bool) {
	l, ok := parseUsageLine(line)
	if !ok {
		return 0, false
	}
	if l.Type == "system" && l.Subtype == "compact_boundary" {
		return l.CompactMetadata.PostTokens, true
	}
	if l.Type == "assistant" && l.hasUsage && !l.IsSidechain {
		u := l.Message.Usage
		if n := u.Input + u.CacheRead + u.CacheCreation; n > 0 {
			return n, true
		}
	}
	return 0, false
}

// lineTTL is the cache lifetime a reply wrote at (3600 or 300 s): false for one that wrote nothing (all of it
// read from cache), a subagent's (5 minutes whatever the session has), or any other line. Claude Code picks the
// main conversation's TTL once per session: 1 h on a subscription within its plan usage, 5 min on usage credits,
// an API key or a cloud provider.
func lineTTL(line []byte) (time.Duration, bool) {
	if !bytes.Contains(line, []byte(`"cache_creation"`)) {
		return 0, false
	}
	l, ok := parseUsageLine(line)
	if !ok || l.Type != "assistant" || l.IsSidechain {
		return 0, false
	}
	w := l.Message.Usage.Writes
	switch {
	case w.Hour > 0:
		return time.Hour, true
	case w.Five > 0:
		return 5 * time.Minute, true
	}
	return 0, false
}

// transcriptUsage reads a session file backwards from its end (a root's grows to tens of MB): the newest reply's
// prompt since the last compaction, and the cache TTL of the newest reply that wrote to the cache, looked for up
// to TTLLookback replies back. Either is unfound (false) when the file has none.
func transcriptUsage(path string, chunk int64) (tokens int64, haveTokens bool, ttl time.Duration, haveTTL bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, 0, false, err
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, false, 0, false, err
	}
	var rest []byte
	replies := 0
	for end > 0 {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return tokens, haveTokens, ttl, haveTTL, err
		}
		lines := bytes.Split(append(buf, rest...), []byte("\n"))
		rest = nil
		if start > 0 { // cut by the chunk: finished with the next one
			rest, lines = lines[0], lines[1:]
		}
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			if !haveTokens {
				tokens, haveTokens = lineTokens(line)
			}
			if !haveTTL {
				ttl, haveTTL = lineTTL(line)
				if bytes.Contains(line, []byte(`"assistant"`)) {
					if _, ok := lineTokens(line); ok {
						replies++
					}
				}
			}
			if haveTokens && (haveTTL || replies > TTLLookback) {
				return tokens, haveTokens, ttl, haveTTL, nil
			}
		}
		end = start
	}
	return tokens, haveTokens, ttl, haveTTL, nil
}

// MeasureContext reads a session's size, its cache's lifetime and whether the cache has expired (the file was
// last written longer ago than the lifetime).
func (a *Adapter) MeasureContext(_ context.Context, req *connect.Request[adapterv1.MeasureContextRequest]) (*connect.Response[adapterv1.MeasureContextResponse], error) {
	m := req.Msg
	path := TranscriptPath(m.Env, m.Cwd, m.SessionId)
	out := &agentmeshv1.ContextUsage{Transcript: path}
	if path == "" {
		return connect.NewResponse(&adapterv1.MeasureContextResponse{Usage: out}), nil
	}
	tokens, haveTokens, ttl, haveTTL, err := transcriptUsage(path, 1<<20)
	if err != nil || !haveTokens {
		return connect.NewResponse(&adapterv1.MeasureContextResponse{Usage: out}), nil
	}
	if !haveTTL {
		ttl = DefaultCacheTTL
	}
	out.Measured, out.Tokens, out.TtlSeconds = true, tokens, int32(ttl/time.Second)
	if st, err := os.Stat(path); err == nil {
		out.WrittenAt = timestamppb.New(st.ModTime())
		out.Cold = time.Since(st.ModTime()) > ttl
	}
	return connect.NewResponse(&adapterv1.MeasureContextResponse{Usage: out}), nil
}

// PlanCompact is a one-off headless turn on the stopped conversation whose only job is `/compact <focus>`: the
// agent's own lean tools, settings and MCP servers, so its prefix is the one the agent cached and the
// conversation isn't handed back to its next launch with the full set, and a 5-minute cache, which is all the
// turn needs. It carries no mesh hooks and no identity (the caller strips AGENT_MESH_*), so it can't message
// anyone or move the agent's state.
func (a *Adapter) PlanCompact(_ context.Context, req *connect.Request[adapterv1.PlanCompactRequest]) (*connect.Response[adapterv1.PlanCompactResponse], error) {
	m := req.Msg
	bin := m.Binary
	if bin == "" {
		bin = "claude"
	}
	argv := []string{bin, "-p", "--resume", m.SessionId}
	if m.Model != "" {
		argv = append(argv, "--model", m.Model)
	}
	argv = append(argv, "--dangerously-skip-permissions")
	if len(m.DenyTools) > 0 {
		argv = append(append(argv, "--disallowedTools"), m.DenyTools...)
	}
	settings := map[string]any{"promptCacheTtl": shortCache}
	if m.Lean != nil {
		argv = append(argv, LeanArgs(m.Lean)...)
		for k, v := range leanSettings(m.Env, m.Lean) {
			settings[k] = v
		}
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	prompt := "/compact"
	if m.Focus != "" {
		prompt += " " + m.Focus
	}
	argv = append(argv, "--settings", string(b), prompt)
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	for _, k := range SessionVars {
		delete(env, k)
	}
	return connect.NewResponse(&adapterv1.PlanCompactResponse{Argv: argv, Env: env}), nil
}
