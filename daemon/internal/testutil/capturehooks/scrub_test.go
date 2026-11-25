package main

import (
	"strings"
	"testing"
)

func TestScrubReplacesHomeRepoAndSessionsConsistently(t *testing.T) {
	s := newScrubber("/home/alice", "/tmp/cap-123", "/tmp/cap-123/repo")
	a, err := s.scrub([]byte(`{"session_id":"AAAAAAAA-1111-4222-8333-444444444444","cwd":"/tmp/cap-123/repo","transcript_path":"/home/alice/.claude/projects/-tmp-cap-123-repo/aaaaaaaa-1111-4222-8333-444444444444.jsonl"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.scrub([]byte(`{"session_id":"bbbbbbbb-1111-4222-8333-444444444444"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"alice", "cap-123", "AAAAAAAA", "aaaaaaaa"} {
		if strings.Contains(string(a), leak) {
			t.Errorf("%q survived: %s", leak, a)
		}
	}
	if !strings.Contains(string(a), `"/home/user/.claude/projects/-work-repo/00000000-0000-4000-8000-000000000001.jsonl"`) ||
		!strings.Contains(string(a), `"cwd": "/work/repo"`) {
		t.Errorf("scrubbed %s", a)
	}
	if !strings.Contains(string(b), "00000000-0000-4000-8000-000000000002") {
		t.Errorf("a second session id: %s", b)
	}
}

func TestScrubReplacesTheWorktreeUnderTheCaptureRoot(t *testing.T) {
	s := newScrubber("/home/alice", "/tmp/cap-123", "/tmp/cap-123/repo")
	out, err := s.scrub([]byte(`{"cwd":"/tmp/cap-123/state/worktrees/r1/repo-capture","scratchpad_dir":"/tmp/claude-1000/-tmp-cap-123-state-worktrees-r1-repo-capture/x"}`))
	if err != nil || strings.Contains(string(out), "cap-123") || !strings.Contains(string(out), "/work/state/worktrees/r1/repo-capture") {
		t.Errorf("%s, %v", out, err)
	}
}

func TestScrubLeavesOutALongSummaryButKeepsAShortReply(t *testing.T) {
	s := newScrubber("/home/alice", "/work")
	long := strings.Repeat("the model's paraphrase of private instructions ", 40)
	out, err := s.scrub([]byte(`{"hook_event_name":"SubagentStop","last_assistant_message":"` + long + `"}`))
	if err != nil || strings.Contains(string(out), "paraphrase of private") || !strings.Contains(string(out), "a conversation summary: left out") {
		t.Errorf("%s, %v", out, err)
	}
	out, err = s.scrub([]byte(`{"hook_event_name":"Stop","last_assistant_message":"# Capture repo"}`))
	if err != nil || !strings.Contains(string(out), "# Capture repo") {
		t.Errorf("a short reply: %s, %v", out, err)
	}
}

func TestScrubReplacesTheScratchFoldersUID(t *testing.T) {
	s := newScrubber("/home/alice", "/work")
	out, err := s.scrub([]byte(`{"scratchpad_dir":"/tmp/claude-1000/-work-x/s"}`))
	if err != nil || strings.Contains(string(out), "1000") || !strings.Contains(string(out), "/tmp/claude-user/") {
		t.Errorf("%s, %v", out, err)
	}
}

func TestScrubRefusesWhatItCannotPlace(t *testing.T) {
	s := newScrubber("/home/alice", "/work")
	s.secrets = []string{"alice"}
	if _, err := s.scrub([]byte(`{"prompt":"hi from Alice"}`)); err == nil {
		t.Error("a user name was kept")
	}
	if _, err := s.scrub([]byte(`{"x":"mail bob@example.com"}`)); err == nil {
		t.Error("an email was kept")
	}
	if _, err := s.scrub([]byte(`not json`)); err == nil {
		t.Error("a non-JSON payload was kept")
	}
}

// A user or host whose name is inside a stand-in ("work", "user") doesn't make every payload refused; the name
// anywhere else still is.
func TestScrubDoesNotTakeItsOwnStandInsForALeak(t *testing.T) {
	s := newScrubber("/home/alice", "/tmp/cap-123", "/tmp/cap-123/repo")
	s.secrets = []string{"work", "user"}
	if out, err := s.scrub([]byte(`{"cwd":"/tmp/cap-123/repo","home":"/home/alice","scratchpad_dir":"/tmp/claude-1000/x"}`)); err != nil {
		t.Fatalf("stand-ins refused: %v\n%s", err, out)
	}
	if _, err := s.scrub([]byte(`{"prompt":"ask work about it"}`)); err == nil {
		t.Error("a user name in a prompt was not refused")
	}
}

// A home that is a symlink shows up under either spelling.
func TestScrubReplacesBothSpellingsOfAHomeThatIsASymlink(t *testing.T) {
	s := newScrubber("/data/home/alice", "/tmp/cap-123", "/tmp/cap-123/repo")
	s.alsoPath("/home/alice", "/home/user")
	out, err := s.scrub([]byte(`{"a":"/home/alice/.claude/x","b":"/data/home/alice/.claude/y","c":"-home-alice-p"}`))
	if err != nil || strings.Contains(string(out), "alice") {
		t.Errorf("%s, %v", out, err)
	}
}
