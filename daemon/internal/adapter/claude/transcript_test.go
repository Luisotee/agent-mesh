package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const (
	kUser   = agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_USER
	kAsst   = agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_ASSISTANT
	kCall   = agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_CALL
	kResult = agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_RESULT
	kMesh   = agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_MESH
)

// Lines in the shapes Claude Code 2.1.x writes (cut down to the fields read).
const (
	lMode     = `{"type":"mode","mode":"normal","sessionId":"s1"}`
	lDoorbell = `{"type":"user","isMeta":true,"uuid":"u1","timestamp":"2026-11-26T19:27:08.201Z","message":{"role":"user","content":"Another Claude session sent a message:\n📬 [mesh] task from lead (ref a1): run ` + "`mesh inbox`" + `\n\nThis came from another Claude session."}}`
	lTyped    = `{"type":"user","uuid":"u2","timestamp":"2026-11-26T19:28:00.000Z","message":{"role":"user","content":"fix the typo <system-reminder>noise</system-reminder>"}}`
	// Injected context that only quotes the doorbell's mark (the user's CLAUDE.md does): not a doorbell, not shown.
	lContext     = `{"type":"user","isMeta":true,"uuid":"u0","message":{"role":"user","content":"<system-reminder>A prompt starting with 📬 [mesh] means you are in a mesh</system-reminder>"}}`
	lQuoting     = `{"type":"user","isMeta":true,"uuid":"u0b","message":{"role":"user","content":"Notes: a line starting with 📬 [mesh] is a doorbell"}}`
	lSkill       = `{"type":"user","isMeta":true,"uuid":"u3","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /x"}]}}`
	lThinking    = `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"abc"}]}}`
	lCall        = `{"type":"assistant","uuid":"a2","timestamp":"2026-11-26T19:28:05.500Z","message":{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"grep -rn\n  typo .","description":"find it"}}]}}`
	lResult      = `{"type":"user","uuid":"u4","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"README.md:3: typo"}]}}`
	lResultList  = `{"type":"user","uuid":"u5","message":{"role":"user","content":[{"tool_use_id":"toolu_2","type":"tool_result","is_error":true,"content":[{"type":"text","text":"boom"},{"type":"image","source":{}}]}]}}`
	lSidechain   = `{"type":"assistant","isSidechain":true,"uuid":"a3","message":{"role":"assistant","content":[{"type":"text","text":"subagent"}]}}`
	lBoundary    = `{"type":"system","subtype":"compact_boundary"}`
	lEdit        = `{"type":"assistant","uuid":"a4","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_3","name":"Edit","input":{"file_path":"/w/README.md","old_string":"a","new_string":"b"}}]}}`
	lUnknownTool = `{"type":"assistant","uuid":"a5","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_4","name":"Odd","input":{"x":1,"y":"z"}}]}}`
)

func entryKinds(es []*agentmeshv1.TranscriptEntry) string {
	var out []string
	for _, e := range es {
		out = append(out, strings.TrimPrefix(e.Kind.String(), "TRANSCRIPT_KIND_"))
	}
	return strings.Join(out, " ")
}

func entries(t *testing.T, line string) []*agentmeshv1.TranscriptEntry {
	t.Helper()
	return TranscriptEntries([]byte(line), 100)
}

func TestTranscriptShowsTheConversationAndNothingElse(t *testing.T) {
	for _, skipped := range []string{lMode, lContext, lQuoting, lSkill, lThinking, lSidechain, lBoundary, `{not json`, ``, `{"type":"user","message":{"content":42}}`} {
		if es := entries(t, skipped); len(es) != 0 {
			t.Errorf("%q gave %v", skipped, es)
		}
	}
	// The doorbell is the mesh's, even though Claude marks it as injected context, and only its first line shows.
	es := entries(t, lDoorbell)
	if len(es) != 1 || es[0].Kind != kMesh || es[0].Text != "📬 [mesh] task from lead (ref a1): run `mesh inbox`" || es[0].Time.AsTime().Year() != 2026 {
		t.Errorf("doorbell: %v", es)
	}
	// What the user typed, without the context Claude added around it.
	if es := entries(t, lTyped); len(es) != 1 || es[0].Kind != kUser || es[0].Text != "fix the typo" || es[0].Id != "u2:0" || es[0].Offset != 100 {
		t.Errorf("typed: %v", es)
	}
	es = entries(t, lCall)
	if entryKinds(es) != "ASSISTANT TOOL_CALL" || es[0].Text != "Looking." || es[1].Tool != "Bash" || es[1].ToolInput != "grep -rn typo ." || es[1].ToolUseId != "toolu_1" || es[1].Id != "a2:1" {
		t.Errorf("call: %v", es)
	}
	if es := entries(t, lResult); len(es) != 1 || es[0].Kind != kResult || es[0].Text != "README.md:3: typo" || es[0].ToolUseId != "toolu_1" || es[0].IsError {
		t.Errorf("result: %v", es)
	}
	if es := entries(t, lResultList); len(es) != 1 || es[0].Text != "boom\n[image]" || !es[0].IsError {
		t.Errorf("result list: %v", es)
	}
}

func TestTranscriptSummarisesWhatATooldWasAskedAndCutsWhatIsTooLong(t *testing.T) {
	if es := entries(t, lEdit); es[0].ToolInput != "/w/README.md" {
		t.Errorf("edit: %q", es[0].ToolInput)
	}
	if es := entries(t, lUnknownTool); es[0].ToolInput != `{"x":1,"y":"z"}` {
		t.Errorf("unknown tool: %q", es[0].ToolInput)
	}
	long := strings.Repeat("é", maxText) // two bytes each: a cut must not split one
	line := fmt.Sprintf(`{"type":"user","uuid":"u","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%q}]}}`, long)
	es := entries(t, line)
	if !es[0].Truncated || len(es[0].Text) > maxText || strings.ContainsRune(es[0].Text, '�') {
		t.Errorf("a long result: %d bytes, truncated %v", len(es[0].Text), es[0].Truncated)
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// numbered makes n assistant lines whose text says their number.
func numbered(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf(`{"type":"assistant","uuid":"a%d","message":{"role":"assistant","content":[{"type":"text","text":"line %03d %s"}]}}`, i, i, strings.Repeat("x", 200)))
	}
	return out
}

func texts(es []*agentmeshv1.TranscriptEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Text[:8])
	}
	return out
}

func TestReadingATranscriptInPagesFromTheEndBackAndOnAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := append([]string{lMode}, numbered(100)...)
	writeLines(t, path, lines...)

	// The tail first: the last entries, not everything, and the way back.
	tail, start, end, atStart, err := ReadTranscript(path, -1, true, 8<<10)
	if err != nil || atStart || start <= 0 || len(tail) < wantEntries || len(tail) >= 100 {
		t.Fatalf("tail: %d entries from %d to %d, atStart %v, %v", len(tail), start, end, atStart, err)
	}
	if last := tail[len(tail)-1].Text[:8]; last != "line 099" {
		t.Errorf("the tail ends at %q", last)
	}
	// Reading back from where it began, until the start: every entry once, in order.
	all := tail
	for !atStart {
		var page []*agentmeshv1.TranscriptEntry
		page, start, _, atStart, err = ReadTranscript(path, start, true, 8<<10)
		if err != nil {
			t.Fatal(err)
		}
		all = append(page, all...)
	}
	if len(all) != 100 || start != 0 || all[0].Text[:8] != "line 000" {
		t.Fatalf("back to the start: %d entries from %d", len(all), start)
	}
	for i, e := range all {
		if !strings.HasPrefix(e.Text, fmt.Sprintf("line %03d", i)) {
			t.Fatalf("entry %d is %q", i, e.Text[:8])
		}
	}

	// Following on: nothing new yet; a line being written is left alone; once whole it comes, once.
	more, s2, e2, _, err := ReadTranscript(path, end, false, 8<<10)
	if err != nil || len(more) != 0 || s2 != end || e2 != end {
		t.Fatalf("nothing new: %v from %d to %d, %v", texts(more), s2, e2, err)
	}
	appendRaw(t, path, `{"type":"assistant","uuid":"n1","message":{"role":"assistant","content":[{"type":"text","text":"new one"`)
	if more, _, e3, _, _ := ReadTranscript(path, end, false, 8<<10); len(more) != 0 || e3 != end {
		t.Fatalf("a half-written line was read: %v, end %d", texts(more), e3)
	}
	appendRaw(t, path, "}]}}\n")
	more, _, e4, _, _ := ReadTranscript(path, end, false, 8<<10)
	if len(more) != 1 || more[0].Text != "new one" || e4 <= end {
		t.Fatalf("the finished line: %v, end %d", texts(more), e4)
	}
	// And a tail read while a line is half written stops before it.
	appendRaw(t, path, `{"type":"assistant","uuid":"n2","message":{"role":"assistant","content":[{"type":"te`)
	tail2, _, e5, _, _ := ReadTranscript(path, -1, true, 8<<10)
	if e5 != e4 || tail2[len(tail2)-1].Text != "new one" {
		t.Errorf("tail with a half line: ends at %d (want %d), last %q", e5, e4, tail2[len(tail2)-1].Text)
	}
}

func TestReadingForwardTakesAPageAtATime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, path, numbered(60)...)
	var got []string
	for off, n := int64(0), 0; ; n++ {
		page, start, end, atStart, err := ReadTranscript(path, off, false, 4<<10)
		if err != nil || start != off || atStart != (off == 0) {
			t.Fatalf("page at %d: start %d, atStart %v, %v", off, start, atStart, err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, texts(page)...)
		off = end
		if n > 100 {
			t.Fatal("never ends")
		}
	}
	if len(got) != 60 || got[0] != "line 000" || got[59] != "line 059" {
		t.Errorf("%d entries: %v ...", len(got), got[:min(3, len(got))])
	}
}

func TestATranscriptThatIsMissingOrWasRewritten(t *testing.T) {
	dir := t.TempDir()
	es, start, end, atStart, err := ReadTranscript(filepath.Join(dir, "none.jsonl"), -1, true, 0)
	if err != nil || len(es) != 0 || start != 0 || end != 0 || !atStart {
		t.Errorf("missing: %v %d %d %v %v", es, start, end, atStart, err)
	}
	path := filepath.Join(dir, "s.jsonl")
	writeLines(t, path, numbered(3)...)
	// An offset past the end (the file is shorter than the reader remembers): it says where the file ends.
	es, _, end, _, err = ReadTranscript(path, 1<<20, false, 0)
	st, _ := os.Stat(path)
	if err != nil || len(es) != 0 || end != st.Size() {
		t.Errorf("rewritten: %v end %d size %d %v", es, end, st.Size(), err)
	}
	// An empty file is an empty conversation.
	writeLines(t, path)
	os.WriteFile(path, nil, 0o600)
	if es, _, _, atStart, err := ReadTranscript(path, -1, true, 0); err != nil || len(es) != 0 || !atStart {
		t.Errorf("empty: %v %v %v", es, atStart, err)
	}
}

func TestALineLongerThanThePageIsFoundBackwardsAndOneLongerThanTheCapIsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	big := fmt.Sprintf(`{"type":"user","uuid":"big","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%q}]}}`, strings.Repeat("y", 40<<10))
	writeLines(t, path, append(append(numbered(2), big), numbered(2)...)...)
	es, _, _, atStart, err := ReadTranscript(path, -1, true, 4<<10)
	if err != nil || len(es) != 5 || !atStart || entryKinds(es) != "ASSISTANT ASSISTANT TOOL_RESULT ASSISTANT ASSISTANT" {
		t.Errorf("a long line in the middle: %s atStart %v %v", entryKinds(es), atStart, err)
	}
	// Forward, with a page smaller than the line: it is read whole, not cut.
	es, _, _, _, err = ReadTranscript(path, 0, false, 1<<10)
	if err != nil || len(es) < 3 || es[2].Kind != kResult {
		t.Errorf("forward over a long line: %s %v", entryKinds(es), err)
	}
}

func TestTranscriptLeavesOutWhatTheCLIWritesForItselfAndShowsACommandAsTyped(t *testing.T) {
	const (
		summary  = `{"type":"user","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"uuid":"c1","message":{"role":"user","content":"This session is being continued from a previous conversation that ran out of context."}}`
		visible  = `{"type":"user","isVisibleInTranscriptOnly":true,"uuid":"c2","message":{"role":"user","content":[{"type":"text","text":"shown in the terminal only"}]}}`
		command  = `{"type":"user","uuid":"c3","message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args>keep the plan</command-args>"}}`
		bare     = `{"type":"user","uuid":"c4","message":{"role":"user","content":"<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>"}}`
		stdout   = `{"type":"user","uuid":"c5","message":{"role":"user","content":"<local-command-stdout>Compacted</local-command-stdout>"}}`
		caveat   = `{"type":"user","isMeta":true,"uuid":"c6","message":{"role":"user","content":"<local-command-caveat>Caveat: the messages below were generated by the user</local-command-caveat>"}}`
		mentions = `{"type":"user","uuid":"c7","message":{"role":"user","content":"what does <command-name> do in the record?"}}`
	)
	for name, line := range map[string]string{"summary": summary, "visible only": visible, "stdout": stdout, "caveat": caveat} {
		if es := entries(t, line); len(es) != 0 {
			t.Errorf("%s shown: %v", name, es)
		}
	}
	for line, want := range map[string]string{command: "/compact keep the plan", bare: "/clear", mentions: "what does <command-name> do in the record?"} {
		es := entries(t, line)
		if len(es) != 1 || es[0].Kind != kUser || es[0].Text != want {
			t.Errorf("%q shown as %v, want the user's %q", line, es, want)
		}
	}
}

// A record line of several MB (a big tool result) used up the backward read's budget before the read could find
// where it began, so "Load earlier" came back unchanged every time.
func TestEveryBackwardPageMovesBackPastALineOfSeveralMB(t *testing.T) {
	big := fmt.Sprintf(`{"type":"user","uuid":"big","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%q}]}}`, strings.Repeat("y", 10<<20))
	for name, lines := range map[string][]string{
		"in the middle": append(append(numbered(1), big), numbered(1)...),
		"the newest":    append(numbered(2), big),
	} {
		path := filepath.Join(t.TempDir(), "s.jsonl")
		writeLines(t, path, lines...)
		var start int64 = -1
		var pages []string
		for page := 0; ; page++ {
			es, from, _, atStart, err := ReadTranscript(path, start, true, 0)
			if err != nil {
				t.Fatal(err)
			}
			pages = append(pages, entryKinds(es))
			if atStart {
				break
			}
			if page > 4 || (page > 0 && from >= start) {
				t.Fatalf("%s: page %d did not move back: from %d, was %d (%q)", name, page, from, start, pages)
			}
			start = from
		}
		if got := strings.Join(pages, "|"); !strings.Contains(got, "TOOL_RESULT") || !strings.Contains(got, "ASSISTANT") {
			t.Errorf("%s: pages %q", name, pages)
		}
	}
}
