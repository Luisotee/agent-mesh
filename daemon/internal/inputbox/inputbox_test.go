package inputbox_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Luisotee/agent-mesh/daemon/internal/inputbox"
	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
)

// repaint draws what a CLI wrote on a terminal of the host's own screen model and returns what the host would
// send a client that attaches: the format Read is made for.
func repaint(t *testing.T, cols, rows int, draw string) []byte {
	t.Helper()
	s := termhost.NewScreen(cols, rows, nil)
	defer s.Close()
	s.Write([]byte(draw))
	return s.Repaint()
}

const (
	dim    = "\x1b[2m"
	grey8  = "\x1b[38;5;8m"
	rgbG   = "\x1b[38;2;128;128;128m"
	reset  = "\x1b[0m"
	orange = "\x1b[38;2;215;119;87m"
)

func rule(cols int) string { return strings.Repeat("─", cols) }

// claudeScreen is Claude Code's box: output, a rule, the prompt line (glyph, NBSP, text), a rule and a footer.
func claudeScreen(prompt string, extra ...string) string {
	out := "working on: x\r\ndone: x\r\n"
	out += rule(60) + "\r\n❯ " + prompt + "\r\n"
	for _, e := range extra {
		out += e + "\r\n"
	}
	return out + rule(60) + "\r\n  ? for shortcuts\r\n"
}

func TestClaudeBox(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   inputbox.State
		text   string
	}{
		{"empty", claudeScreen(""), inputbox.Empty, ""},
		{"a dim placeholder is no draft", claudeScreen(dim + `Try "fix lint errors"` + reset), inputbox.Empty, ""},
		{"typed text is a draft", claudeScreen("please look at the thing"), inputbox.Typed, "please look at the thing"},
		{"a coloured slash command counts as typed", claudeScreen(orange + "/model" + reset), inputbox.Typed, "/model"},
		{"typed text before a dim hint", claudeScreen("hello " + dim + "world" + reset), inputbox.Typed, "hello"},
		{"a wrapped draft", claudeScreen("first line", "  second line"), inputbox.Typed, "first line second line"},
		{"our own doorbell left in the box", claudeScreen("📬 [mesh] task from lead (ref abc123): run `mesh inbox`"), inputbox.Typed,
			"📬 [mesh] task from lead (ref abc123): run `mesh inbox`"},
		{"a dialog has no box", "Quick safety check: do you trust this folder?\r\n❯ 1. No, exit\r\n  2. Yes\r\n", inputbox.NoBox, ""},
		{"no closing rule", "out\r\n" + rule(60) + "\r\n❯ text\r\nfooter\r\n", inputbox.Unclear, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := inputbox.Read(repaint(t, 80, 24, c.screen), inputbox.Claude, "abc123")
			if got.State != c.want || got.Text != c.text {
				t.Fatalf("got %v %q, want %v %q", got.State, got.Text, c.want, c.text)
			}
			if wantDraft := c.want == inputbox.Typed && !strings.HasPrefix(c.text, inputbox.DoorbellPrefix); got.Draft() != wantDraft {
				t.Errorf("Draft() = %v, want %v", got.Draft(), wantDraft)
			}
		})
	}
}

func TestAPanelBelowTheBoxDoesNotMoveIt(t *testing.T) {
	// agy draws a background-tasks panel under the box, with rules of its own further down.
	screen := claudeScreen("a draft") + "  tasks:\r\n" + rule(60) + "\r\n  1 running\r\n"
	if got := inputbox.Read(repaint(t, 80, 24, screen), inputbox.Claude, "x"); got.State != inputbox.Typed || got.Text != "a draft" {
		// The search is from the bottom: the lower rule sits above no prompt line, so the box above it is found.
		t.Fatalf("got %v %q", got.State, got.Text)
	}
}

func TestAgyGreyPlaceholderIsEmptyButNotForClaude(t *testing.T) {
	screen := claudeScreen(grey8 + "Auto mode: edits & safe commands auto-approved (shift+tab)" + reset)
	raw := repaint(t, 80, 24, screen)
	if got := inputbox.Read(raw, inputbox.Agy, "x"); got.State != inputbox.Empty {
		t.Errorf("agy: %v %q, want empty", got.State, got.Text)
	}
	if got := inputbox.Read(raw, inputbox.Claude, "x"); got.State != inputbox.Typed {
		t.Errorf("claude: %v, want typed (colour 8 is not its ghost style)", got.State)
	}
	// SGR 90 is the same grey.
	if got := inputbox.Read(repaint(t, 80, 24, claudeScreen("\x1b[90mgrey hint"+reset)), inputbox.Agy, "x"); got.State != inputbox.Empty {
		t.Errorf("SGR 90: %v", got.State)
	}
}

func TestTheTokenCountsOnlyAboveTheBox(t *testing.T) {
	screen := "> 📬 [mesh] task from lead (ref abc123): run `mesh inbox`\r\nworking on it\r\n" + claudeScreen("")
	if got := inputbox.Read(repaint(t, 80, 24, screen), inputbox.Claude, "abc123"); got.TokenAbove != 1 || got.State != inputbox.Empty {
		t.Errorf("echoed above the box: %+v", got)
	}
	// Typed in the box and not submitted: not counted.
	typed := claudeScreen("📬 [mesh] task from lead (ref abc123): run `mesh inbox`")
	if got := inputbox.Read(repaint(t, 80, 24, typed), inputbox.Claude, "abc123"); got.TokenAbove != 0 || !got.OurDoorbell() {
		t.Errorf("in the box: %+v", got)
	}
	// An older doorbell of the same sender doesn't count for a new token.
	if got := inputbox.Read(repaint(t, 80, 24, screen), inputbox.Claude, "fffeee"); got.TokenAbove != 0 {
		t.Errorf("another token: %+v", got)
	}
	// A wrap that splits the token across two rows still counts.
	wrapped := "📬 [mesh] task from lead (re\r\nf abc123): run\r\n" + claudeScreen("")
	if got := inputbox.Read(repaint(t, 80, 24, wrapped), inputbox.Claude, "abc123"); got.TokenAbove != 1 {
		t.Errorf("wrapped: %+v", got)
	}
	// No box: never confirms.
	if got := inputbox.Read(repaint(t, 80, 24, "📬 (ref abc123)\r\n"), inputbox.Claude, "abc123"); got.TokenAbove != 0 || got.State != inputbox.NoBox {
		t.Errorf("no box: %+v", got)
	}
}

// openCodeScreen is OpenCode's gutter box: rows of `┃`, a status row after a blank one, and a `╹▀▀…` bottom, the
// status wrapped or padded the way a narrow pane does.
func openCodeScreen(cols int, prompt string, statusRows int) string {
	out := "some output\r\n┃ a quoted line of output\r\n\r\n"
	out += "┃  " + prompt + "\r\n┃\r\n"
	status := "Build  Big Pickle OpenCode Zen"
	for i := 0; i < statusRows; i++ {
		out += "┃  " + status + "\r\n"
		status = ""
	}
	return out + "╹" + strings.Repeat("▀", cols-1) + "\r\n"
}

func TestOpenCodeGutterBox(t *testing.T) {
	for _, cols := range []int{50, 70, 90} {
		for _, status := range []int{1, 2} {
			name := fmt.Sprintf("%d cols, status rows %d", cols, status)
			t.Run(name, func(t *testing.T) {
				for _, c := range []struct {
					prompt string
					want   inputbox.State
					text   string
				}{
					{rgbG + `Ask anything… "Fix a TODO in the codebase"` + reset, inputbox.Empty, ""},
					{"Ask anything...", inputbox.Empty, ""}, // the placeholder is empty whatever colour it is drawn in
					{"fix the bug", inputbox.Typed, "fix the bug"},
					{"", inputbox.Empty, ""},
				} {
					got := inputbox.Read(repaint(t, cols, 30, openCodeScreen(cols, c.prompt, status)), inputbox.OpenCode, "x")
					if got.State != c.want || got.Text != c.text {
						t.Errorf("%q: got %v %q, want %v %q", c.prompt, got.State, got.Text, c.want, c.text)
					}
				}
			})
		}
	}
}

func TestOpenCodeStatusWithNoBlankRowAboveItIsUnclear(t *testing.T) {
	screen := "┃  Build  Big Pickle\r\n╹" + strings.Repeat("▀", 40) + "\r\n"
	if got := inputbox.Read(repaint(t, 60, 20, screen), inputbox.OpenCode, "x"); got.State != inputbox.Unclear {
		t.Errorf("got %v", got.State)
	}
	if got := inputbox.Read(repaint(t, 60, 20, "no box here\r\n"), inputbox.OpenCode, "x"); got.State != inputbox.NoBox {
		t.Errorf("got %v", got.State)
	}
}

func TestOpenCodeTokenAboveTheGutterBox(t *testing.T) {
	screen := "┃ 📬 [mesh] info from lead (ref 0a1b2c): run `mesh inbox`\r\n" + openCodeScreen(60, rgbG+"Ask anything…"+reset, 1)
	got := inputbox.Read(repaint(t, 60, 30, screen), inputbox.OpenCode, "0a1b2c")
	if got.TokenAbove != 1 || got.State != inputbox.Empty {
		t.Errorf("%+v", got)
	}
}

func TestColonFormSGRIsReadNotTakenForAReset(t *testing.T) {
	// A curly underline (`4:3`) in the middle of a dim placeholder doesn't reset it.
	dimmed := dim + "Try " + "\x1b[4:3m" + "this" + reset
	if got := inputbox.Read(repaint(t, 80, 24, claudeScreen(dimmed)), inputbox.Claude, "x"); got.State != inputbox.Empty {
		t.Errorf("%v %q", got.State, got.Text)
	}
	// Colour 8 and a mid grey in the colon form are ghost text where the CLI draws it so.
	for _, sgr := range []string{"\x1b[38:5:8m", "\x1b[38:2::128:128:128m", "\x1b[38:2:128:128:128m", "\x1b[38;2;128;128;128;1m"} {
		if got := inputbox.Read(repaint(t, 80, 24, claudeScreen(sgr+"a hint"+reset)), inputbox.Profile{Ghost: inputbox.GhostDim | inputbox.GhostGrey | inputbox.GhostRGBGrey}, "x"); got.State != inputbox.Empty {
			t.Errorf("%q: %v %q", sgr, got.State, got.Text)
		}
	}
}

func TestAnEscapeSequenceWithIntermediateBytesIsSkippedWhole(t *testing.T) {
	// ESC ( B selects a charset: the "(" is an intermediate and "B" the final byte, none of it text. A final byte
	// that is a multi-byte character is skipped whole too, not cut in the middle.
	for _, seq := range []string{"\x1b(B", "\x1b#8", "\x1b=", "\x1bé"} {
		raw := "\x1b[1;1H" + rule(60) + "\x1b[2;1H❯ " + seq + "hi" + "\x1b[3;1H" + rule(60) + "\x1b[4;1H  ? for shortcuts"
		if got := inputbox.Read([]byte(raw), inputbox.Claude, "x"); got.State != inputbox.Typed || got.Text != "hi" {
			t.Errorf("%q: %v %q, want typed %q", seq, got.State, got.Text, "hi")
		}
	}
}
