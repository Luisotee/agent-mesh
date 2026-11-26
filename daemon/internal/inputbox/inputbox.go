// Package inputbox reads what an agent CLI's input box holds from a terminal repaint (what the terminal host
// renders for a client that has no screen), the way bin/mesh's `typed_text` and `doorbells_shown` read a herdr
// pane (design §3, "a human's draft holds the doorbell"). It tells an empty box (ghost text such as a placeholder
// is not typed text) from a draft someone typed, and counts a doorbell's `(ref <token>)` above the box, which is
// how a typed doorbell is known to have been submitted.
//
// Anything it can't tell is Unclear or NoBox, never a draft: the hold fails open, so a doorbell is delayed by a
// draft it positively sees and never by one it only suspects.
package inputbox

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DoorbellPrefix starts every doorbell. Text in the box that starts with it is the daemon's own, left by an
// earlier try: not a human's draft.
const DoorbellPrefix = "📬 [mesh]"

// State is what the input box holds.
type State int

const (
	// Empty: a box with nothing typed in it (ghost text such as a placeholder doesn't count).
	Empty State = iota + 1
	// Typed: text is typed in it (Reading.Text).
	Typed
	// NoBox: no input box on the screen: a dialog, an alternate screen, a CLI still starting.
	NoBox
	// Unclear: a box was found that can't be read (no closing rule, a status block that can't be told from text).
	Unclear
)

func (s State) String() string {
	switch s {
	case Empty:
		return "empty"
	case Typed:
		return "typed"
	case NoBox:
		return "no-box"
	case Unclear:
		return "unclear"
	}
	return "unknown"
}

// Reading is what a screen's input box shows.
type Reading struct {
	State State
	// Text is what is typed (State Typed), with the whitespace squeezed.
	Text string
	// TokenAbove counts the copies of the doorbell's `ref<token>` above the input box, whitespace removed so no
	// wrap can split it. Zero when no box was found: what can't be told apart from the input box never confirms.
	TokenAbove int
}

// Draft reports whether the box holds a human's draft: typed text that isn't one of our doorbells.
func (r Reading) Draft() bool {
	return r.State == Typed && !strings.HasPrefix(r.Text, DoorbellPrefix)
}

// OurDoorbell reports whether the box holds a doorbell of ours, typed and not submitted.
func (r Reading) OurDoorbell() bool {
	return r.State == Typed && strings.HasPrefix(r.Text, DoorbellPrefix)
}

// Ghost is how a CLI draws ghost text in an empty box.
type Ghost uint8

const (
	// GhostDim is SGR 2, Claude's `Try …` placeholder.
	GhostDim Ghost = 1 << iota
	// GhostGrey is foreground colour 8 (`38;5;8`, or SGR 90).
	GhostGrey
	// GhostRGBGrey is a true-colour grey foreground (`38;2;128;128;128`, or a near-grey of mid luminance).
	GhostRGBGrey
)

// Profile is how one CLI draws its input box.
type Profile struct {
	Ghost Ghost
	// Gutter is a `┃`-gutter box down to a `╹▀▀…` bottom (OpenCode's), instead of a prompt glyph between two rules.
	Gutter bool
	// StatusRows are rows at the foot of a gutter box that aren't typed text.
	StatusRows int
	// StatusBlock is a gutter box's status of however many rows, after a blank one, at its foot.
	StatusBlock bool
	// Placeholder is an empty box's hint, read as empty whatever colour it is drawn in.
	Placeholder *regexp.Regexp
}

// The profiles of the CLIs with an adapter.
var (
	// Claude: the rules box; its `Try …` placeholder is dim (a coloured slash command isn't, so it counts as typed).
	Claude = Profile{Ghost: GhostDim}
	// Agy: the rules box; its empty box shows "Auto mode: edits & safe commands …" in colour 8, not dim.
	Agy = Profile{Ghost: GhostDim | GhostGrey}
	// OpenCode: a gutter box whose foot is the agent and model, one row or several in a narrow pane with blank ones
	// around it; its placeholder is grey (`38;2;128;128;128`).
	OpenCode = Profile{Ghost: GhostDim | GhostGrey | GhostRGBGrey, Gutter: true, StatusBlock: true,
		Placeholder: regexp.MustCompile(`^Ask anything(…|\.\.\.)\s*("[^"]*")?$`)}
)

var (
	ruleRE  = regexp.MustCompile(`^\s*─{10,}\s*$`)
	inputRE = regexp.MustCompile(`^[>❯]`)
)

// Read reads a repaint.
func Read(raw []byte, p Profile, token string) Reading {
	return readRows(parse(raw), p, token)
}

func readRows(rows []row, p Profile, token string) Reading {
	plain := make([]string, len(rows))
	for i, r := range rows {
		plain[i] = r.plain()
	}
	var above []string
	var typed string
	var state State
	if p.Gutter {
		start, last, ok := gutterBox(plain)
		if !ok {
			return Reading{State: NoBox}
		}
		above = plain[:start]
		typed, state = gutterTyped(rows[start:last+1], p)
	} else {
		at := inputBoxAt(plain)
		if at < 0 {
			return Reading{State: NoBox}
		}
		above = plain[:at-1]
		typed, state = rulesTyped(rows, plain, at, p)
	}
	out := Reading{State: state, TokenAbove: strings.Count(strings.Join(strings.Fields(strings.Join(above, "")), ""), "ref"+token)}
	if state == Typed {
		out.Text = typed
	}
	return out
}

// inputBoxAt is the index of the agent's input line (the prompt at column 0 under a rule line), searched from the
// bottom: a panel can follow the box (agy's background tasks), so counting rules would misplace it. -1: none.
func inputBoxAt(plain []string) int {
	for i := len(plain) - 2; i >= 0; i-- {
		if ruleRE.MatchString(plain[i]) && inputRE.MatchString(plain[i+1]) {
			return i + 1
		}
	}
	return -1
}

// rulesTyped is what is typed from the prompt line down to the box's closing rule. A box with no closing rule
// (what follows may be a footer) can't be told: Unclear.
func rulesTyped(rows []row, plain []string, at int, p Profile) (string, State) {
	var typed []string
	for n := at; n < len(rows); n++ {
		if n > at && ruleRE.MatchString(plain[n]) {
			return finish(typed, p)
		}
		cells := rows[n].cells
		if n == at {
			cells = cells[1:] // the glyph
		}
		typed = append(typed, rows[n].typed(cells, p))
	}
	return "", Unclear
}

func finish(typed []string, p Profile) (string, State) {
	text := squeeze(strings.Join(typed, " "))
	if text == "" || (p.Placeholder != nil && p.Placeholder.MatchString(text)) {
		return "", Empty
	}
	return text, Typed
}

// gutterBox is the (first, last) row of a `┃`-gutter input box: its last `┃` row just above a `╹` bottom, found
// from the bottom (output above can hold `┃` rows of its own, but no bottom after them).
func gutterBox(plain []string) (int, int, bool) {
	for end := len(plain) - 1; end > 0; end-- {
		if strings.HasPrefix(strings.TrimLeft(plain[end], " \t"), "╹") && strings.HasPrefix(strings.TrimLeft(plain[end-1], " \t"), "┃") {
			start := end - 1
			for start > 0 && strings.HasPrefix(strings.TrimLeft(plain[start-1], " \t"), "┃") {
				start--
			}
			return start, end - 1, true
		}
	}
	return 0, 0, false
}

// gutterTyped is what follows each row's gutter, ghost text left out, without the box's status: its last
// StatusRows rows, or with StatusBlock the last run of non-blank rows, after any blank ones at the foot. A narrow
// pane wraps that line onto several rows or pads blank ones after it, so a count of rows would misread it as
// typed text. A block with no blank row above it can't be told from text: Unclear.
func gutterTyped(box []row, p Profile) (string, State) {
	if p.StatusBlock {
		blank := make([]bool, len(box))
		for i, r := range box {
			blank[i] = strings.TrimSpace(afterGutter(r.plain())) == ""
		}
		end := len(box)
		for end > 0 && blank[end-1] {
			end--
		}
		start := end
		for start > 0 && !blank[start-1] {
			start--
		}
		if start == 0 || start == end {
			return "", Unclear // no status below a blank row to leave out
		}
		box = box[:start]
	} else if p.StatusRows > 0 {
		if p.StatusRows > len(box) {
			return "", Unclear
		}
		box = box[:len(box)-p.StatusRows]
	}
	var typed []string
	for _, r := range box {
		at := -1
		for i, c := range r.cells {
			if c.ch == '┃' {
				at = i
				break
			}
		}
		if at < 0 {
			return "", Unclear
		}
		typed = append(typed, r.typed(r.cells[at+1:], p))
	}
	return finish(typed, p)
}

func afterGutter(s string) string {
	_, after, _ := strings.Cut(s, "┃")
	return after
}

func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// ── the repaint ──

// A cell is one visible character with how it was drawn.
type cell struct {
	ch                  rune
	dim, grey8, rgbGrey bool
}

type row struct{ cells []cell }

func (r row) plain() string {
	var b strings.Builder
	for _, c := range r.cells {
		b.WriteRune(c.ch)
	}
	return b.String()
}

func (p Profile) ghost(c cell) bool {
	return (c.dim && p.Ghost&GhostDim != 0) ||
		(c.grey8 && p.Ghost&(GhostGrey|GhostRGBGrey) != 0) ||
		(c.rgbGrey && p.Ghost&GhostRGBGrey != 0)
}

// typed is a row's cells as text, ghost text as spaces.
func (r row) typed(cells []cell, p Profile) string {
	var b strings.Builder
	for _, c := range cells {
		if p.ghost(c) {
			b.WriteByte(' ')
		} else {
			b.WriteRune(c.ch)
		}
	}
	return b.String()
}

// parse reads the repaint the terminal host sends a client (`Screen.Data`): mode switches, then each row of
// the screen after a cursor move to it (`ESC [ <row> ; 1 H`), drawn with SGR styles and the odd OSC (a hyperlink),
// and a last cursor move. Only the characters and their styles matter here.
func parse(raw []byte) []row {
	var rows []row
	cur := 0 // the row characters go to
	var dim, grey8, rgbGrey bool
	put := func(ch rune) {
		for len(rows) <= cur {
			rows = append(rows, row{})
		}
		rows[cur].cells = append(rows[cur].cells, cell{ch: ch, dim: dim, grey8: grey8, rgbGrey: rgbGrey})
	}
	s := string(raw)
	for i := 0; i < len(s); {
		ch, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case ch == 0x1b && i+1 < len(s) && s[i+1] == '[':
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j >= len(s) {
				i = len(s)
				continue
			}
			params, final := s[i+2:j], s[j]
			switch final {
			case 'm':
				sgr(params, &dim, &grey8, &rgbGrey)
			case 'H', 'f':
				if r, _, _ := strings.Cut(params, ";"); r != "" {
					if n, err := strconv.Atoi(r); err == nil && n >= 1 {
						cur = n - 1
					}
				} else {
					cur = 0
				}
			}
			i = j + 1
		case ch == 0x1b && i+1 < len(s) && s[i+1] == ']':
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
				j++
			}
			if j < len(s) && s[j] == 0x1b {
				j++
			}
			i = j + 1
		case ch == 0x1b:
			i += 2
		case ch < 0x20 || ch == 0x7f:
			i += size
		default:
			put(ch)
			i += size
		}
	}
	return rows
}

// sgr applies one SGR sequence's parameters to the three styles that matter (bin/mesh's `visible_chars`).
func sgr(params string, dim, grey8, rgbGrey *bool) {
	parts := strings.Split(params, ";")
	p := make([]int, len(parts))
	for i, s := range parts {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			p[i] = n
		}
	}
	at := func(i int) int {
		if i < len(p) {
			return p[i]
		}
		return -1
	}
	for i := 0; i < len(p); {
		switch n := p[i]; {
		case n == 38 || n == 48 || n == 58: // an extended colour: its index or RGB values aren't attributes
			if n == 38 {
				*grey8 = at(i+1) == 5 && at(i+2) == 8
				*rgbGrey = false
				if at(i+1) == 2 && i+4 < len(p) {
					r, g, b := p[i+2], p[i+3], p[i+4]
					hi, lo := max(r, g, b), min(r, g, b)
					mean := float64(r+g+b) / 3
					*rgbGrey = hi-lo <= 24 && mean >= 96 && mean <= 160
				}
			}
			switch at(i + 1) {
			case 5:
				i += 3
			case 2:
				i += 5
			default:
				i++
			}
		case n == 0:
			*dim, *grey8, *rgbGrey = false, false, false
			i++
		case n == 22:
			*dim = false
			i++
		case n == 2:
			*dim = true
			i++
		case n == 90:
			*grey8, *rgbGrey = true, false
			i++
		case (n >= 30 && n <= 37) || (n >= 91 && n <= 97) || n == 39: // another foreground, or the default one
			*grey8, *rgbGrey = false, false
			i++
		default:
			i++
		}
	}
}
