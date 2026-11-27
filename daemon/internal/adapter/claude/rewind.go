package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
)

// CapRewind is the capability of a CLI whose conversation the daemon can take back to before a prompt
// (RewindAgent): a copy of its session cut there, resumed under a new id.
const CapRewind = "rewind"

// ErrNotAPrompt says the entry named is not a prompt of the session (another kind of line, or none at all).
var ErrNotAPrompt = errors.New("not a prompt of this conversation")

// ErrFirstPrompt says no assistant reply comes before the prompt (it is the conversation's first, or only a root's
// /clear precedes it): Claude Code (2.1.332, checked live) can't resume a session with no messages ("No conversation
// found"), so there is nothing to rewind to.
var ErrFirstPrompt = errors.New("no answered turn comes before that prompt to go back to")

// maxSessionLine bounds one line of a session file (a tool result can be large).
const maxSessionLine = 64 << 20

// CutSession writes a copy of the session at path cut before the user prompt whose line has the uuid given, as
// session newID beside it, and returns that prompt's text. Claude Code resumes a conversation from its last line back
// up the parentUuid chain, so the copy keeps, of the lines before the prompt, those on the prompt's own chain (its
// parent and up, through a compaction's logicalParentUuid) and the lines that are on no chain (the session's
// bookkeeping): a branch left by an earlier rewind, or a sidechain, is not resumed. Each line kept has its sessionId
// set to newID (checked live with Claude Code 2.1.332: the copy resumes without what was cut). The original is left
// as it was.
func CutSession(path, uuid, newID string) (text string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxSessionLine)
	type line struct {
		raw       []byte
		uuid      string
		assistant bool
	}
	var before []line
	parents := map[string]string{}
	parent, found, linear := "", false, false
	for sc.Scan() {
		raw := append([]byte(nil), sc.Bytes()...)
		var l struct {
			Type          string          `json:"type"`
			UUID          string          `json:"uuid"`
			Parent        json.RawMessage `json:"parentUuid"`
			LogicalParent json.RawMessage `json:"logicalParentUuid"`
			IsMeta        bool            `json:"isMeta"`
			Message       json.RawMessage `json:"message"`
		}
		_ = json.Unmarshal(raw, &l)
		var p string
		if json.Unmarshal(l.Parent, &p) != nil || p == "" {
			_ = json.Unmarshal(l.LogicalParent, &p)
		}
		if l.UUID != "" && l.UUID == uuid {
			var ok bool
			if text, ok = promptText(l.Message); l.Type != "user" || l.IsMeta || !ok {
				return "", ErrNotAPrompt
			}
			// A record with no parentUuid at all is read in file order (a CLI that keeps no chain).
			parent, found, linear = p, true, len(l.Parent) == 0
			break
		}
		if l.UUID != "" {
			parents[l.UUID] = p
		}
		before = append(before, line{raw: raw, uuid: l.UUID, assistant: l.Type == "assistant"})
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotAPrompt
	}
	chain := map[string]bool{}
	for at := parent; at != "" && !chain[at]; at = parents[at] {
		chain[at] = true
	}
	var out bytes.Buffer
	answered := false
	for _, l := range before {
		if !linear && l.uuid != "" && !chain[l.uuid] {
			continue
		}
		answered = answered || l.assistant
		kept, err := withSession(l.raw, newID)
		if err != nil {
			return "", err
		}
		out.Write(kept)
		out.WriteByte('\n')
	}
	if !answered {
		return "", ErrFirstPrompt
	}
	dst := filepath.Join(filepath.Dir(path), newID+".jsonl")
	if err := fsutil.WriteAtomic(dst, out.Bytes(), 0o600); err != nil {
		return "", fmt.Errorf("writing the rewound conversation: %w", err)
	}
	return text, nil
}

// RemoveCut removes the copy CutSession wrote for newID beside path (a rewind that was refused after the cut).
func RemoveCut(path, newID string) {
	_ = os.Remove(filepath.Join(filepath.Dir(path), newID+".jsonl"))
}

// withSession is a session line with its sessionId set to id; a line that isn't a JSON object, or has no
// sessionId, is kept as it is.
func withSession(line []byte, id string) ([]byte, error) {
	var o map[string]json.RawMessage
	if json.Unmarshal(line, &o) != nil {
		return append([]byte(nil), line...), nil
	}
	if _, ok := o["sessionId"]; !ok {
		return append([]byte(nil), line...), nil
	}
	v, _ := json.Marshal(id)
	o["sessionId"] = v
	return json.Marshal(o)
}

// promptText is what the user typed, as the chat shows it (injected reminders and a command's tags dropped, `userEntry`),
// and whether the line is a prompt: a string content, or text and image blocks. A line holding a tool result answers
// a tool call, not the user, even with text beside it: cut there, the copy would end on a call with no result.
func promptText(msg json.RawMessage) (string, bool) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return "", false
	}
	typed := func(text string) (string, bool) {
		e := userEntry(text, false)
		if e == nil || e.Kind != agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_USER {
			return "", false
		}
		return e.Text, true
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return typed(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return "", false
	}
	var parts []string
	isPrompt := false
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			return "", false
		case "text":
			if t, ok := typed(b.Text); ok {
				parts = append(parts, t)
				isPrompt = true
			}
		case "image":
			isPrompt = true
		}
	}
	return strings.Join(parts, "\n"), isPrompt
}
