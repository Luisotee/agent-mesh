package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCutSessionKeepsWhatCameBeforeThePrompt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.jsonl")
	lines := []string{
		`{"type":"session-owner","sessionId":"old"}`,
		`{"type":"user","uuid":"u1","sessionId":"old","message":{"role":"user","content":"remember ALPHA"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"old","message":{"content":[{"type":"text","text":"OK"}]}}`,
		`{"type":"queue-operation","sessionId":"old"}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"old","message":{"role":"user","content":[{"type":"text","text":"remember"},{"type":"image"},{"type":"text","text":"BETA"}]}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"old","message":{"content":[{"type":"text","text":"OK"}]}}`,
		`not json`,
	}
	if err := os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := CutSession(src, "u2", "new")
	if err != nil {
		t.Fatal(err)
	}
	if text != "remember\nBETA" {
		t.Errorf("text = %q", text)
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	kept := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(kept) != 4 {
		t.Fatalf("kept %d lines, want the 4 before the prompt:\n%s", len(kept), got)
	}
	for i, l := range kept {
		var o map[string]any
		if err := json.Unmarshal([]byte(l), &o); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if o["sessionId"] != "new" {
			t.Errorf("line %d keeps sessionId %v", i, o["sessionId"])
		}
	}
	if !strings.Contains(kept[2], `"parentUuid":"u1"`) {
		t.Errorf("the chain changed: %s", kept[2])
	}
	if orig, _ := os.ReadFile(src); string(orig) != strings.Join(lines, "\n")+"\n" {
		t.Error("the original changed")
	}
}

func TestCutSessionRefusesALineThatIsNotAPrompt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.jsonl")
	body := `{"type":"user","uuid":"u1","sessionId":"old","message":{"content":"hi"}}
{"type":"assistant","uuid":"a1","sessionId":"old","message":{"content":[]}}
{"type":"user","uuid":"m1","isMeta":true,"sessionId":"old","message":{"content":"caveat"}}
`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a1", "m1", "missing"} {
		if _, err := CutSession(src, id, "new"); !errors.Is(err, ErrNotAPrompt) {
			t.Errorf("%s: err = %v, want ErrNotAPrompt", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "new.jsonl")); !os.IsNotExist(err) {
		t.Error("a refused cut wrote a file")
	}
}
