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

// Copilot's gutter box: a `╻▄▄` top, `┃` rows and a `╹▀▀` bottom, the placeholder dim.
func TestCopilotGutterBox(t *testing.T) {
	screen := func(rows ...string) string {
		out := "some output\r\n┃ a quoted line of output\r\n\r\n╻" + strings.Repeat("▄", 40) + "\r\n"
		for _, r := range rows {
			out += "┃ " + r + "\r\n"
		}
		return out + "╹" + strings.Repeat("▀", 40) + "\r\n"
	}
	for _, c := range []struct {
		name string
		rows []string
		want inputbox.State
		text string
	}{
		{"the dim placeholder is no draft", []string{dim + "Type @ to mention files, / for commands" + reset}, inputbox.Empty, ""},
		{"the placeholder is empty whatever colour it is drawn in", []string{"Type @ to mention files, / for commands"}, inputbox.Empty, ""},
		{"an empty box", []string{""}, inputbox.Empty, ""},
		{"a draft", []string{"half a thought"}, inputbox.Typed, "half a thought"},
		{"a draft over two rows", []string{"half a thought", "and more"}, inputbox.Typed, "half a thought and more"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := inputbox.Read(repaint(t, 60, 30, screen(c.rows...)), inputbox.Copilot, "x")
			if got.State != c.want || got.Text != c.text {
				t.Errorf("got %v %q, want %v %q", got.State, got.Text, c.want, c.text)
			}
		})
	}
}

// A Claude Code 2.1.332 screen as the lead's terminal showed it, idle, its box holding the suggested prompt (rows 59 to 61 of
// a 249x63 repaint, the rules cut to 120 columns; the rows above were the conversation). The suggestion is dim (`\x1b[2m`).
const claudeSuggestion = "\x1b[?25l\x1b[m\x1b[H\x1b[2J" +
	"\x1b[1;1H\x1b[38;2;136;136;136m" + "────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────" + "\x1b[m\x1b[m" +
	"\x1b[2;1H❯ \x1b[2mrebuild the windows installer when 202 merges\x1b[m\x1b[m" +
	"\x1b[3;1H\x1b[38;2;136;136;136m" + "────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────" + "\x1b[m\x1b[m" +
	"\x1b[4;1H  \x1b[38;2;255;107;128m⏵⏵\x1b[m \x1b[38;2;255;107;128mbypass\x1b[m \x1b[38;2;255;107;128mpermissions\x1b[m \x1b[38;2;255;107;128mon\x1b[m"

// The ghost text of an empty box when Claude's terminal has the focus: it paints its cursor on the text by inverting the
// first character (`Cr()` in the 2.1.332 binary: `invert(p[0]) + dim(p.slice(1))`, invert being `\x1b[7m` and `\x1b[27m`).
func focused(ghost string) string {
	r := []rune(ghost)
	return "\x1b[7m" + string(r[:1]) + "\x1b[27m" + dim + string(r[1:]) + reset
}

func TestClaudeSuggestedPromptIsEmptyWithOrWithoutTheCursorOnIt(t *testing.T) {
	if got := inputbox.Read([]byte(claudeSuggestion), inputbox.Claude, "x"); got.State != inputbox.Empty {
		t.Errorf("the captured screen: %v %q, want empty", got.State, got.Text)
	}
	for _, ghost := range []string{
		"rebuild the windows installer when 202 merges",            // the suggested prompt
		`Try "fix lint errors"`,                                    // the placeholder
		"Press up to edit queued messages, Enter to send them now", // the hint while messages wait
		"Message @reviewer…",
	} {
		got := inputbox.Read(repaint(t, 120, 24, claudeScreen(focused(ghost))), inputbox.Claude, "x")
		if got.State != inputbox.Empty || got.Draft() {
			t.Errorf("%q with the cursor on it: %v %q, want empty", ghost, got.State, got.Text)
		}
	}
}

func TestADraftWithTheCursorOnItIsStillADraft(t *testing.T) {
	for name, text := range map[string]string{
		"cursor at the end":              "fix the typo\x1b[7m \x1b[27m",
		"cursor on the first character":  "\x1b[7mf\x1b[27mix the typo",
		"cursor in the middle":           "fix \x1b[7mt\x1b[27mhe typo",
		"a draft before an inline ghost": "/com" + dim + "pact" + reset,
		"one character, cursor on it":    "\x1b[7mx\x1b[27m",
	} {
		got := inputbox.Read(repaint(t, 120, 24, claudeScreen(text)), inputbox.Claude, "x")
		if got.State != inputbox.Typed || !got.Draft() {
			t.Errorf("%s: %v %q, want a draft", name, got.State, got.Text)
		}
	}
}

func TestClaudesSuggestedPromptIsTheBoxsGhostText(t *testing.T) {
	const want = "rebuild the windows installer when 202 merges"
	// The lead's own capture (unfocused: all of it dim), and the same drawn with the cursor on its first character.
	if got := inputbox.Read([]byte(claudeSuggestion), inputbox.Claude, "x"); got.State != inputbox.Empty || got.Ghost != want {
		t.Errorf("the captured screen: %v %q", got.State, got.Ghost)
	}
	if got := inputbox.Read(repaint(t, 120, 24, claudeScreen(focused(want))), inputbox.Claude, "x"); got.State != inputbox.Empty || got.Ghost != want {
		t.Errorf("with the cursor on it: %v %q, want %q", got.State, got.Ghost, want)
	}
	// A suggestion longer than the box is wrapped by the CLI at a word onto more rows (indented under the glyph): one text.
	long := "rebuild the windows installer once 202 merges, then run the packaged app and check the tray and the notifications"
	wrapped := claudeScreen(focused("rebuild the windows installer once 202 merges, then run the"), "  "+dim+"packaged app and check the tray and the notifications"+reset)
	if got := inputbox.Read(repaint(t, 80, 24, wrapped), inputbox.Claude, "x"); got.State != inputbox.Empty || got.Ghost != long {
		t.Errorf("wrapped: %v %q, want %q", got.State, got.Ghost, long)
	}
}

func TestTheCLIsOwnFixedHintsAreNoSuggestionAndADraftHasNoGhost(t *testing.T) {
	for _, hint := range []string{
		`Try "fix lint errors"`,
		"Press up to edit queued messages, Enter to send them now",
		"Message @reviewer…",
	} {
		for _, draw := range []string{focused(hint), dim + hint + reset} {
			if got := inputbox.Read(repaint(t, 120, 24, claudeScreen(draw)), inputbox.Claude, "x"); got.State != inputbox.Empty || got.Ghost != "" {
				t.Errorf("%q: %v ghost %q, want none", hint, got.State, got.Ghost)
			}
		}
	}
	// A draft is a draft, with an inline completion's grey after it too; an empty box has no ghost.
	for _, draw := range []string{"fix the typo", "/com" + dim + "pact" + reset, ""} {
		if got := inputbox.Read(repaint(t, 120, 24, claudeScreen(draw)), inputbox.Claude, "x"); got.Ghost != "" {
			t.Errorf("%q: ghost %q", draw, got.Ghost)
		}
	}
	// Only a CLI whose profile says it suggests has one: agy's grey hint is not a prediction.
	if got := inputbox.Read(repaint(t, 120, 24, claudeScreen(grey8+"Auto mode: edits & safe commands auto-approved"+reset)), inputbox.Agy, "x"); got.State != inputbox.Empty || got.Ghost != "" {
		t.Errorf("agy: %v %q", got.State, got.Ghost)
	}
}
