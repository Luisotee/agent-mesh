package termhost

import (
	"regexp"
	"strings"
	"testing"
)

var sgrRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return sgrRE.ReplaceAllString(s, "") }

func repaintOf(t *testing.T, writes ...string) string {
	t.Helper()
	s := NewScreen(60, 4, nil)
	defer s.Close()
	for _, w := range writes {
		s.Write([]byte(w))
	}
	return string(s.Repaint())
}

// Claude Code 2.1.332 sets its window title from the prompt line, `ESC ] 0 ; ✳ Claude Code BEL`; the emulator
// printed " Claude Code" after the glyph, which the input box reader took for a draft (found live).
func TestScreenDropsAWindowTitleWithNonASCII(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
	}{
		{"BEL", []string{"❯ \x1b]0;✳ Claude Code\a"}},
		{"ST", []string{"❯ \x1b]0;◑ Claude Code\x1b\\"}},
		{"split across writes", []string{"❯ \x1b]0;✳ Cla", "ude Code", "\a"}},
		{"split in the introducer", []string{"❯ \x1b", "]0;✳ Claude Code\a"}},
		{"split before the ST", []string{"❯ \x1b]0;✳ Claude Code\x1b", "\\"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := repaintOf(t, tc.writes...)
			if strings.Contains(got, "Claude Code") {
				t.Fatalf("the title was drawn: %q", got)
			}
			if !strings.Contains(got, "❯ ") {
				t.Fatalf("the prompt is gone: %q", got)
			}
		})
	}
}

func TestScreenKeepsTextAroundAndASCIIOSC(t *testing.T) {
	got := plain(repaintOf(t, "before \x1b[1mbold\x1b[m \x1b]0;✳ title\a after \x1b]0;ascii title\a done"))
	if !strings.Contains(got, "before bold  after  done") {
		t.Fatalf("text around the title was lost: %q", got)
	}
	if strings.Contains(got, "title") {
		t.Fatalf("a title was drawn: %q", got)
	}
}

func TestScreenPassesANonOSCEscapeThrough(t *testing.T) {
	got := plain(repaintOf(t, "a\x1b[31mred\x1b[m\x1b7\x1b[2;1Hx\x1b8"))
	if !strings.Contains(got, "ared") || !strings.Contains(got, "\x1b[2;1Hx") {
		t.Fatalf("escapes did not reach the emulator: %q", got)
	}
}

func TestOSCFilterGivesUpOnAStringThatNeverEnds(t *testing.T) {
	var f oscFilter
	in := "\x1b]0;" + strings.Repeat("é", oscMax)
	out := string(f.filter([]byte(in)))
	if out == "" || f.state != 0 {
		t.Fatalf("an endless OSC was held for good: out %d bytes, state %d", len(out), f.state)
	}
}

// An OSC cut short by another escape sequence loses its text, not the sequence that cut it.
func TestScreenKeepsTheEscapeSequenceThatCutsAnOSCShort(t *testing.T) {
	got := plain(repaintOf(t, "abc\x1b]0;✳ x\x1b[2Kdef"))
	if strings.Contains(got, "2K") || !strings.Contains(got, "def") {
		t.Fatalf("the CSI after the OSC was drawn as text: %q", got)
	}
}

// An unterminated OSC cut short by a new `ESC ]` hands over to that OSC: its non-ASCII text is dropped too.
func TestScreenFiltersAnOSCThatAnotherOSCCutsShort(t *testing.T) {
	for _, in := range []string{
		"abc\x1b]0;ascii\x1b]0;✳ title\a def",
		"abc\x1b]0;✳ a\x1b]0;✳ b\a def",
		"abc\x1b]0;✳ a\x1b", // split: the second introducer arrives in the next write
	} {
		writes := []string{in}
		if strings.HasSuffix(in, "\x1b") {
			writes = []string{in, "]0;✳ b\a def"}
		}
		got := plain(repaintOf(t, writes...))
		if strings.Contains(got, "title") || strings.Contains(got, " a") && strings.Contains(got, "✳") || strings.Contains(got, " b") || !strings.Contains(got, "def") {
			t.Errorf("%q: the second title was drawn or the text lost: %q", in, got)
		}
	}
}

func TestOSCFilterRestartsOnANewIntroducer(t *testing.T) {
	var f oscFilter
	out := string(f.filter([]byte("\x1b]0;ascii\x1b]0;✳ x")))
	if out != "\x1b]0;ascii\x1b\\" || f.state != 2 || string(f.held) != "\x1b]0;✳ x" || !f.wide {
		t.Fatalf("out %q, state %d, held %q, wide %v", out, f.state, f.held, f.wide)
	}
	if out = string(f.filter([]byte("\x07z"))); out != "z" || f.state != 0 {
		t.Fatalf("out %q, state %d", out, f.state)
	}
}
