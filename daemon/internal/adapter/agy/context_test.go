package agy

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

func pbVarint(n uint64) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}

func pbInt(num int, n uint64) []byte { return append(pbVarint(uint64(num)<<3), pbVarint(n)...) }

func pbBytes(num int, b []byte) []byte {
	return append(append(pbVarint(uint64(num)<<3|2), pbVarint(uint64(len(b)))...), b...)
}

// stepMetadata is a step's metadata with a usage message in field 9: fresh input (2), output (3), cached input (5).
func stepMetadata(fresh, out, cached uint64) []byte {
	usage := append(append(pbInt(2, fresh), pbInt(3, out)...), pbInt(5, cached)...)
	return append(pbBytes(1, []byte("x")), pbBytes(9, usage)...)
}

type step struct {
	typ, status int
	meta        []byte
}

func writeConversation(t *testing.T, dir, id string, steps ...step) string {
	t.Helper()
	path := filepath.Join(dir, id+".db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, status INTEGER, metadata BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i, s := range steps {
		if _, err := db.Exec(`INSERT INTO steps (idx, step_type, status, metadata) VALUES (?,?,?,?)`, i, s.typ, s.status, s.meta); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func measureAgy(t *testing.T, dir, id string) *adapterv1.MeasureContextResponse {
	t.Helper()
	res, err := New().MeasureContext(context.Background(), connect.NewRequest(&adapterv1.MeasureContextRequest{
		SessionId: id, Env: map[string]string{"AGENT_MESH_AGY_CONVERSATIONS": dir}}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func TestAgyMeasureNewestCallFreshPlusCached(t *testing.T) {
	dir := t.TempDir()
	writeConversation(t, dir, "c1",
		step{14, 3, stepMetadata(100, 5, 900)},
		step{15, 3, nil},
		step{14, 3, stepMetadata(40, 7, 60000)}, // newest call: 60040
		step{15, 3, []byte{0xff, 0xff}},         // not protobuf: skipped
	)
	u := measureAgy(t, dir, "c1").Usage
	if !u.Measured || u.Tokens != 60040 || u.Cold {
		t.Fatalf("usage %+v", u)
	}
}

func TestAgyMeasureCompactionStepIsZeroOnlyWhenDone(t *testing.T) {
	dir := t.TempDir()
	writeConversation(t, dir, "done",
		step{14, 3, stepMetadata(1, 1, 90000)},
		step{compactionStep, stepDone, nil})
	if u := measureAgy(t, dir, "done").Usage; !u.Measured || u.Tokens != 0 {
		t.Fatalf("a finished compaction is size 0: %+v", u)
	}
	writeConversation(t, dir, "failed",
		step{14, 3, stepMetadata(1, 1, 90000)},
		step{compactionStep, 4, nil}) // cancelled: compacted nothing
	if u := measureAgy(t, dir, "failed").Usage; u.Tokens != 90001 {
		t.Fatalf("a failed compaction compacts nothing: %+v", u)
	}
}

func TestAgyMeasureUnmeasured(t *testing.T) {
	dir := t.TempDir()
	if u := measureAgy(t, dir, "nope").Usage; u.Measured {
		t.Fatalf("missing file: %+v", u)
	}
	writeConversation(t, dir, "empty", step{15, 3, nil})
	if u := measureAgy(t, dir, "empty").Usage; u.Measured {
		t.Fatalf("no usage anywhere: %+v", u)
	}
	if u := measureAgy(t, dir, "").Usage; u.Measured {
		t.Fatalf("no id: %+v", u)
	}
}

func TestAgyMeasureCold(t *testing.T) {
	dir := t.TempDir()
	path := writeConversation(t, dir, "c1", step{14, 3, stepMetadata(1, 1, 5)})
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if u := measureAgy(t, dir, "c1").Usage; !u.Cold {
		t.Fatalf("a db untouched for two hours is cold: %+v", u)
	}
}

func TestAgyPlanCompact(t *testing.T) {
	res, err := New().PlanCompact(context.Background(), connect.NewRequest(&adapterv1.PlanCompactRequest{
		SessionId: "c1", Model: "gemini-4-neon-high", Focus: "Keep the repo facts"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agy", "--conversation", "c1", "--model", "gemini-4-neon-high", "--dangerously-skip-permissions", "-p", "/compact Keep the repo facts"}
	if len(res.Msg.Argv) != len(want) {
		t.Fatalf("argv %q", res.Msg.Argv)
	}
	for i := range want {
		if res.Msg.Argv[i] != want[i] {
			t.Fatalf("argv %q", res.Msg.Argv)
		}
	}
}

// A tier-spawned agy agent's model is a family; `agy --model flash` is refused (live), so the compaction runs the
// id the launch would.
func TestAgyPlanCompactResolvesAFamilyToAnID(t *testing.T) {
	a := New()
	a.ListModels = func(context.Context, string, map[string]string) (string, error) { return sampleListing, nil }
	res, err := a.PlanCompact(context.Background(), connect.NewRequest(&adapterv1.PlanCompactRequest{
		SessionId: "c1", Model: "hydrogen", Effort: "high", Focus: "Keep the repo facts"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Msg.Argv, " "); !strings.Contains(got, "--model gemini-4-hydrogen-high ") {
		t.Fatalf("argv %q", got)
	}
}

func TestProtobufFieldsRejectsTruncated(t *testing.T) {
	if _, err := protobufFields(pbBytes(9, []byte("abcdef"))[:4]); err == nil {
		t.Fatal("a cut message must be an error")
	}
}

func TestAgyPlanCompactDropsTheSessionVars(t *testing.T) {
	res, err := New().PlanCompact(context.Background(), connect.NewRequest(&adapterv1.PlanCompactRequest{
		SessionId: "c1", Model: "gemini-4-neon-high", Env: map[string]string{SessionVars[0]: "x", "KEEP": "1"}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Msg.Env[SessionVars[0]]; ok || res.Msg.Env["KEEP"] != "1" {
		t.Fatalf("env %v", res.Msg.Env)
	}
}
