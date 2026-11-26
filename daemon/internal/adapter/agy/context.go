package agy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// Context size (docs/design.md §12): agy documents no format. Its conversation file is SQLite, and field 9 of a
// step's `metadata` protobuf is that call's usage: 2 the input read fresh, 5 the input read from cache, 3 the
// output. Checked against `agy -p --output-format json`, whose `usage` sums them over the whole conversation (so
// it can't tell the size).

const (
	// compactionStep is a step holding a compaction's summary: a `/compact`, or agy's own.
	compactionStep = 23
	// stepDone is a step's status once it has finished (tool steps that failed or were cancelled have others).
	stepDone = 3
	// cacheTTL: agy's prompt cache has no documented lifetime; an hour, as Claude's default.
	cacheTTL = time.Hour
)

type field struct {
	num   int
	n     uint64 // a varint's value
	bytes []byte // everything else
	isInt bool
}

var errProto = errors.New("not a protobuf message")

// protobufFields splits one protobuf message into its fields: varints as numbers, the rest as bytes.
func protobufFields(blob []byte) ([]field, error) {
	varint := func(i int) (uint64, int, error) {
		var n uint64
		for shift := uint(0); ; shift += 7 {
			if i >= len(blob) || shift > 63 {
				return 0, i, errProto
			}
			b := blob[i]
			n |= uint64(b&0x7f) << shift
			i++
			if b&0x80 == 0 {
				return n, i, nil
			}
		}
	}
	var out []field
	for i := 0; i < len(blob); {
		key, j, err := varint(i)
		if err != nil {
			return nil, err
		}
		i = j
		f := field{num: int(key >> 3)}
		switch key & 7 {
		case 0:
			f.n, i, err = varint(i)
			if err != nil {
				return nil, err
			}
			f.isInt = true
		case 1, 5:
			size := 8
			if key&7 == 5 {
				size = 4
			}
			if i+size > len(blob) {
				return nil, errProto
			}
			f.bytes, i = blob[i:i+size], i+size
		case 2:
			var size uint64
			size, i, err = varint(i)
			if err != nil || size > uint64(len(blob)-i) {
				return nil, errProto
			}
			f.bytes, i = blob[i:i+int(size)], i+int(size)
		default:
			return nil, errProto
		}
		out = append(out, f)
	}
	return out, nil
}

// conversationFile is where agy keeps a conversation: `<agy home>/conversations/<id>.db`.
func conversationFile(env map[string]string, session string) string {
	if session == "" {
		return ""
	}
	if d := env["AGENT_MESH_AGY_CONVERSATIONS"]; d != "" {
		return filepath.Join(d, session+".db")
	}
	home := env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "conversations", session+".db")
}

// conversationTokens is a conversation's context size: the prompt of its last model call since its last
// compaction. False when the file is missing or reads otherwise.
func conversationTokens(path string) (int64, bool) {
	if _, err := os.Stat(path); err != nil {
		return 0, false
	}
	uri := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.Clean(path))
	db, err := sql.Open("sqlite", "file:"+uri+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return 0, false
	}
	defer db.Close()
	// Newest first, read only as far as the first call or compaction.
	rows, err := db.Query(`SELECT step_type, status, metadata FROM steps ORDER BY idx DESC`)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	for rows.Next() {
		var stepType, status sql.NullInt64
		var metadata []byte
		if rows.Scan(&stepType, &status, &metadata) != nil {
			continue
		}
		if stepType.Int64 == compactionStep && status.Int64 == stepDone { // a failed one compacted nothing
			return 0, true
		}
		if len(metadata) == 0 {
			continue
		}
		top, err := protobufFields(metadata)
		if err != nil {
			continue
		}
		var usage []byte
		for _, f := range top {
			if f.num == 9 && !f.isInt {
				usage = f.bytes
				break
			}
		}
		counts, err := protobufFields(usage)
		if err != nil {
			continue
		}
		var fresh, cached uint64
		haveFresh := false
		for _, f := range counts {
			switch {
			case f.num == 2 && f.isInt:
				fresh, haveFresh = f.n, true
			case f.num == 5 && f.isInt:
				cached = f.n
			}
		}
		if haveFresh {
			return int64(fresh + cached), true
		}
	}
	return 0, false
}

// MeasureContext reads a conversation's size. Cold is whether the file (or its WAL) was last written more than
// an hour ago.
func (a *Adapter) MeasureContext(_ context.Context, req *connect.Request[adapterv1.MeasureContextRequest]) (*connect.Response[adapterv1.MeasureContextResponse], error) {
	m := req.Msg
	path := conversationFile(m.Env, m.SessionId)
	out := &agentmeshv1.ContextUsage{Transcript: path, TtlSeconds: int32(cacheTTL / time.Second)}
	if path != "" {
		if n, ok := conversationTokens(path); ok {
			out.Measured, out.Tokens = true, n
			var newest time.Time
			for _, p := range []string{path, path + "-wal"} {
				if st, err := os.Stat(p); err == nil && st.ModTime().After(newest) {
					newest = st.ModTime()
				}
			}
			if !newest.IsZero() {
				out.WrittenAt = timestamppb.New(newest)
			}
			out.Cold = !newest.IsZero() && time.Since(newest) > cacheTTL
		}
	}
	return connect.NewResponse(&adapterv1.MeasureContextResponse{Usage: out}), nil
}

// PlanCompact is a headless `/compact <focus>` on the conversation.
func (a *Adapter) PlanCompact(_ context.Context, req *connect.Request[adapterv1.PlanCompactRequest]) (*connect.Response[adapterv1.PlanCompactResponse], error) {
	m := req.Msg
	if m.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("no conversation to compact"))
	}
	bin := m.Binary
	if bin == "" {
		bin = "agy"
	}
	prompt := "/compact"
	if m.Focus != "" {
		prompt += " " + m.Focus
	}
	argv := []string{bin, "--conversation", m.SessionId}
	if m.Model != "" {
		argv = append(argv, "--model", m.Model)
	}
	argv = append(argv, "--dangerously-skip-permissions", "-p", prompt)
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	return connect.NewResponse(&adapterv1.PlanCompactResponse{Argv: argv, Env: env}), nil
}
