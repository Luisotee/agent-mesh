package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// Progress is what a Claude Code session file says of how far an agent is: the todo list it keeps (the TodoWrite tool) and the
// size of its conversation.
type Progress struct {
	Plan          []PlanItem
	ContextTokens int64
}

// PlanItem is one entry of the agent's todo list.
type PlanItem struct {
	Content string
	// "pending", "in_progress" or "completed".
	Status string
}

// maxPlanItems bounds the list a client is sent; a todo list is a dozen lines, never hundreds.
const maxPlanItems = 50

// maxTodoScan is how far back from the end of the file a TodoWrite is looked for: a root's session grows to tens of MB, and a
// plan older than this is not the plan of what the agent does now.
const maxTodoScan = 16 << 20

// ReadProgress reads a session file backwards from its end: the newest TodoWrite of the main conversation (a subagent's
// list, in a sidechain line, is another conversation's), and the newest reply's prompt since the last compaction. A file that
// can't be read is an error; one with neither is an empty Progress.
func ReadProgress(path string) (Progress, error) {
	var p Progress
	tokens, have, _, _, err := transcriptUsage(path, 1<<20)
	if err != nil {
		return p, err
	}
	if have {
		p.ContextTokens = tokens
	}
	p.Plan, err = lastTodos(path, 1<<20)
	return p, err
}

type todoLine struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// lastTodos finds the newest TodoWrite call in the file, reading it from the end in chunks; nil when there is none.
// The scan stops at the first one: a call replaces the whole list, so the newest is the plan.
func lastTodos(path string, chunk int64) ([]PlanItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	var rest []byte
	floor := end - maxTodoScan
	for end > 0 && end > floor {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, err
		}
		lines := bytes.Split(append(buf, rest...), []byte("\n"))
		rest = nil
		if start > 0 {
			rest, lines = lines[0], lines[1:]
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if items, ok := todosOfLine(lines[i]); ok {
				return items, nil
			}
		}
		end = start
	}
	return nil, nil
}

// todosOfLine is the list a line's TodoWrite call holds: false for any other line.
func todosOfLine(line []byte) ([]PlanItem, bool) {
	if !bytes.Contains(line, []byte(`"TodoWrite"`)) {
		return nil, false
	}
	var l todoLine
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.IsSidechain {
		return nil, false
	}
	var blocks []contentBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return nil, false
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		if b.Type != "tool_use" || b.Name != "TodoWrite" {
			continue
		}
		var in struct {
			Todos []struct {
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"todos"`
		}
		if json.Unmarshal(b.Input, &in) != nil {
			return nil, false
		}
		var out []PlanItem
		for _, t := range in.Todos {
			c := strings.TrimSpace(t.Content)
			if c == "" || len(out) >= maxPlanItems {
				continue
			}
			st := t.Status
			if st != "in_progress" && st != "completed" {
				st = "pending"
			}
			if len(c) > 200 {
				c = strings.ToValidUTF8(c[:200], "") // cut inside a character: a proto string must stay valid UTF-8
			}
			out = append(out, PlanItem{Content: c, Status: st})
		}
		return out, true
	}
	return nil, false
}
