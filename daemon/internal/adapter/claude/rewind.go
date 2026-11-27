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

	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
)

// CapRewind is the capability of a CLI whose conversation the daemon can take back to before a prompt
// (RewindAgent): a copy of its session cut there, resumed under a new id.
const CapRewind = "rewind"

// ErrNotAPrompt says the entry named is not a prompt of the session (another kind of line, or none at all).
var ErrNotAPrompt = errors.New("not a prompt of this conversation")

// maxSessionLine bounds one line of a session file (a tool result can be large).
const maxSessionLine = 64 << 20

// CutSession writes a copy of the session at path cut before the user prompt whose line has the uuid given, as
// session newID beside it, and returns that prompt's text. Every line kept is the original's, with its sessionId
// set to newID; Claude Code resumes a conversation from its last line back up the parentUuid chain, which a cut at
// a line boundary keeps whole (checked live with Claude Code 2.1.332: the copy resumes without what was cut). The
// original is left as it was.
func CutSession(path, uuid, newID string) (text string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxSessionLine)
	var out bytes.Buffer
	found := false
	for sc.Scan() {
		line := sc.Bytes()
		var l struct {
			Type    string          `json:"type"`
			UUID    string          `json:"uuid"`
			IsMeta  bool            `json:"isMeta"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &l) == nil && l.UUID == uuid {
			if l.Type != "user" || l.IsMeta {
				return "", ErrNotAPrompt
			}
			text = promptText(l.Message)
			found = true
			break
		}
		kept, err := withSession(line, newID)
		if err != nil {
			return "", err
		}
		out.Write(kept)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotAPrompt
	}
	dst := filepath.Join(filepath.Dir(path), newID+".jsonl")
	if err := fsutil.WriteAtomic(dst, out.Bytes(), 0o600); err != nil {
		return "", fmt.Errorf("writing the rewound conversation: %w", err)
	}
	return text, nil
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

// promptText is what the user typed: a string content, or its text blocks joined.
func promptText(msg json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
