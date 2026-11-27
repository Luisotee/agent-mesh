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
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"old","message":{"role":"user","content":"remember ALPHA"}}`,
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

func TestCutSessionFollowsThePromptsChainPastAnAbandonedBranch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.jsonl")
	// P2 was rewound once in the terminal: P2b's parent is A1, and P2/A2 are a branch nobody resumes.
	lines := []string{
		`{"type":"user","uuid":"p1","parentUuid":null,"sessionId":"old","message":{"content":"one"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"p1","sessionId":"old","message":{"content":[]}}`,
		`{"type":"user","uuid":"p2","parentUuid":"a1","sessionId":"old","message":{"content":"two"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"p2","sessionId":"old","message":{"content":[]}}`,
		`{"type":"last-prompt","sessionId":"old"}`,
		`{"type":"user","uuid":"p2b","parentUuid":"a1","sessionId":"old","message":{"content":"two again"}}`,
		`{"type":"user","uuid":"r1","parentUuid":"p2b","sessionId":"old","message":{"content":[{"type":"tool_result","content":"x"}]}}`,
	}
	if err := os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CutSession(src, "r1", "x"); !errors.Is(err, ErrNotAPrompt) {
		t.Errorf("a tool result line: %v, want ErrNotAPrompt", err)
	}
	if _, err := CutSession(src, "p2b", "new"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "new.jsonl"))
	var uuids []string
	for _, l := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		var o map[string]any
		_ = json.Unmarshal([]byte(l), &o)
		if u, _ := o["uuid"].(string); u != "" {
			uuids = append(uuids, u)
		}
	}
	if strings.Join(uuids, ",") != "p1,a1" {
		t.Errorf("kept %v, want p1,a1 (the branch p2/a2 dropped)", uuids)
	}
	if !strings.Contains(string(got), "last-prompt") {
		t.Error("a line on no chain was dropped")
	}
}

func TestCutSessionReturnsThePromptAsTheChatShowsItAndRefusesAToolResultLine(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.jsonl")
	body := `{"type":"user","uuid":"u1","sessionId":"old","message":{"content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"old","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}
{"type":"user","uuid":"r1","parentUuid":"a1","sessionId":"old","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"x"},{"type":"text","text":"typed mid-turn"}]}}
{"type":"user","uuid":"u2","parentUuid":"r1","sessionId":"old","message":{"content":[{"type":"text","text":"fix the typo"},{"type":"text","text":"<system-reminder>noise</system-reminder>"}]}}
{"type":"user","uuid":"c1","parentUuid":"u2","sessionId":"old","message":{"content":"<command-name>/foo</command-name><command-args>bar</command-args>"}}
`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CutSession(src, "r1", "n1"); !errors.Is(err, ErrNotAPrompt) {
		t.Errorf("a tool result with text beside it: err = %v, want ErrNotAPrompt", err)
	}
	if text, err := CutSession(src, "u2", "n2"); err != nil || text != "fix the typo" {
		t.Errorf("text, err = %q, %v; want the typed text without the reminder", text, err)
	}
	if text, err := CutSession(src, "c1", "n3"); err != nil || text != "/foo bar" {
		t.Errorf("a slash command: text, err = %q, %v; want /foo bar", text, err)
	}
}

func TestCutSessionRefusesTheFirstPrompt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.jsonl")
	body := `{"type":"session-owner","sessionId":"old"}
{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"old","message":{"content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"old","message":{"content":[]}}
`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CutSession(src, "u1", "new"); !errors.Is(err, ErrFirstPrompt) {
		t.Errorf("err = %v, want ErrFirstPrompt", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.jsonl")); !os.IsNotExist(err) {
		t.Error("a refused cut wrote a file")
	}
}

func TestCutSessionRefusesAPromptWithNoAnsweredTurnBeforeIt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.jsonl")
	// A root after a reset: a caveat, the /clear line and a system line (all with uuids), then the first real prompt.
	body := `{"type":"user","uuid":"c0","isMeta":true,"parentUuid":null,"sessionId":"old","message":{"content":"caveat"}}
{"type":"user","uuid":"c1","parentUuid":"c0","sessionId":"old","message":{"content":"<command-name>/clear</command-name>"}}
{"type":"system","uuid":"s1","parentUuid":"c1","sessionId":"old"}
{"type":"user","uuid":"u1","parentUuid":"s1","sessionId":"old","message":{"content":"first"}}
`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "u1"} {
		if _, err := CutSession(src, id, "new"); !errors.Is(err, ErrFirstPrompt) {
			t.Errorf("%s: err = %v, want ErrFirstPrompt", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "new.jsonl")); !os.IsNotExist(err) {
		t.Error("a refused cut wrote a file")
	}
}
