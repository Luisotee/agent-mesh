package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func todoWriteLine(t *testing.T, sidechain bool, todos ...map[string]string) string {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"todos": todos})
	b, err := json.Marshal(map[string]any{
		"type": "assistant", "isSidechain": sidechain,
		"message": map[string]any{"content": []map[string]any{{"type": "tool_use", "name": "TodoWrite", "input": json.RawMessage(in)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeSession(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadProgressTakesTheNewestTodoWriteOfTheMainConversation(t *testing.T) {
	p := writeSession(t,
		todoWriteLine(t, false, map[string]string{"content": "old", "status": "pending"}),
		todoWriteLine(t, false, map[string]string{"content": "one", "status": "completed"}, map[string]string{"content": "two", "status": "in_progress"}, map[string]string{"content": "three", "status": "weird"}),
		todoWriteLine(t, true, map[string]string{"content": "a subagent's", "status": "pending"}),
	)
	got, err := ReadProgress(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []PlanItem{{"one", "completed"}, {"two", "in_progress"}, {"three", "pending"}}
	if len(got.Plan) != len(want) {
		t.Fatalf("plan = %v, want %v", got.Plan, want)
	}
	for i := range want {
		if got.Plan[i] != want[i] {
			t.Errorf("item %d = %v, want %v", i, got.Plan[i], want[i])
		}
	}
}

// A long item is cut to 200 bytes, and a cut inside a multi-byte character leaves valid UTF-8: a proto string with a
// broken character fails the whole response.
func TestAPlanItemCutInsideACharacterStaysValidUTF8(t *testing.T) {
	long := strings.Repeat("a", 199) + "é" + "tail" // the 200th byte is the first half of é
	p := writeSession(t, todoWriteLine(t, false, map[string]string{"content": long, "status": "pending"}))
	got, err := ReadProgress(p)
	if err != nil || len(got.Plan) != 1 {
		t.Fatalf("%v, %v", got, err)
	}
	if c := got.Plan[0].Content; !utf8.ValidString(c) || len(c) > 200 || !strings.HasPrefix(long, c) {
		t.Errorf("content %q (%d bytes)", c, len(c))
	}
}

func TestReadProgressWithoutATodoWriteIsEmpty(t *testing.T) {
	got, err := ReadProgress(writeSession(t, `{"type":"user"}`))
	if err != nil || len(got.Plan) != 0 {
		t.Errorf("%v, %v", got, err)
	}
}
