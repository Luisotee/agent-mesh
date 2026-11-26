package termhost

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Ring keeps the newest output bytes of a terminal, numbered from 0 since the terminal started.
type Ring struct {
	buf   []byte
	start uint64 // the number of the oldest byte kept
	end   uint64 // one past the newest byte
}

// NewRing returns a ring keeping up to size bytes.
func NewRing(size int) *Ring { return &Ring{buf: make([]byte, 0, size)} }

// Append adds output.
func (r *Ring) Append(p []byte) {
	capacity := cap(r.buf)
	if len(p) >= capacity {
		r.buf = append(r.buf[:0], p[len(p)-capacity:]...)
		r.end += uint64(len(p))
		r.start = r.end - uint64(capacity)
		return
	}
	if over := len(r.buf) + len(p) - capacity; over > 0 {
		copy(r.buf, r.buf[over:])
		r.buf = r.buf[:len(r.buf)-over]
		r.start += uint64(over)
	}
	r.buf = append(r.buf, p...)
	r.end += uint64(len(p))
}

// Start is the number of the oldest byte kept; End is one past the newest.
func (r *Ring) Start() uint64 { return r.start }
func (r *Ring) End() uint64   { return r.end }

// Since returns a copy of the bytes from offset to the end, or false when offset is no longer (or not yet) in
// the ring.
func (r *Ring) Since(offset uint64) ([]byte, bool) {
	if offset < r.start || offset > r.end {
		return nil, false
	}
	return append([]byte(nil), r.buf[offset-r.start:]...), true
}

// Screen is the host's model of what a terminal shows, fed the same bytes as the ring. It answers terminal
// queries (DA1/DA2, cursor position) through the reply function it was made with, and renders a repaint for a
// client that has no screen. Callers serialize every method.
type Screen interface {
	Write(p []byte)
	Resize(cols, rows int)
	// Repaint returns bytes that make a fresh terminal of the same size show the same screen: the mode
	// preamble, the screen's content, and the cursor.
	Repaint() []byte
	// Preamble is the mode preamble alone.
	Preamble() []byte
	Close()
}

// NewScreenFunc makes a terminal's screen; reply receives its answers to queries (nil drops them).
type NewScreenFunc func(cols, rows int, reply func([]byte)) Screen

// DEC private modes a client must have set to behave like the program expects. The alternate screen (47,
// 1047, 1049) and the cursor's visibility (25) are handled on their own.
var preambleModes = map[int]bool{
	1:    true,                                                             // application cursor keys
	1000: true, 1002: true, 1003: true, 1005: true, 1006: true, 1015: true, // mouse reporting
	1004: true, // focus events
	2004: true, // bracketed paste
}

type vtScreen struct {
	emu     *vt.Emulator
	osc     oscFilter
	modes   map[int]bool // DEC private modes the program set
	closing atomic.Bool
	pumped  chan struct{}
}

// NewScreen returns the charmbracelet/x/vt-backed screen.
func NewScreen(cols, rows int, reply func([]byte)) Screen {
	s := &vtScreen{emu: vt.NewEmulator(cols, rows), modes: map[int]bool{}, pumped: make(chan struct{})}
	s.emu.SetCallbacks(vt.Callbacks{
		EnableMode: func(m ansi.Mode) {
			if dm, ok := m.(ansi.DECMode); ok {
				s.modes[int(dm)] = true
			}
		},
		DisableMode: func(m ansi.Mode) {
			if dm, ok := m.(ansi.DECMode); ok {
				delete(s.modes, int(dm))
			}
		},
	})
	go s.pump(reply)
	return s
}

// pump drains the emulator's answers; the emulator writes them to a pipe, so its Write blocks until they are
// read.
func (s *vtScreen) pump(reply func([]byte)) {
	defer close(s.pumped)
	buf := make([]byte, 4096)
	for {
		n, err := s.emu.Read(buf)
		if s.closing.Load() || err != nil {
			return
		}
		if n > 0 && reply != nil {
			reply(append([]byte(nil), buf[:n]...))
		}
	}
}

func (s *vtScreen) Write(p []byte) {
	if out := s.osc.filter(p); len(out) > 0 {
		s.emu.Write(out)
	}
}
func (s *vtScreen) Resize(cols, rows int) { s.emu.Resize(cols, rows) }

// Close stops the pump, then the emulator. The emulator's Close and Read race on an unguarded flag, so the
// pump is woken with a query of our own and gone before Close runs.
func (s *vtScreen) Close() {
	if s.closing.Swap(true) {
		return
	}
	s.emu.Write([]byte("\x1b[c"))
	<-s.pumped
	s.emu.Close()
}

func (s *vtScreen) Preamble() []byte {
	var b strings.Builder
	if s.emu.IsAltScreen() {
		b.WriteString("\x1b[?1049h")
	}
	modes := make([]int, 0, len(s.modes))
	for m := range s.modes {
		if preambleModes[m] {
			modes = append(modes, m)
		}
	}
	sort.Ints(modes)
	for _, m := range modes {
		fmt.Fprintf(&b, "\x1b[?%dh", m)
	}
	return []byte(b.String())
}

func (s *vtScreen) Repaint() []byte {
	var b strings.Builder
	b.Write(s.Preamble())
	b.WriteString("\x1b[?25l\x1b[m\x1b[H\x1b[2J")
	for i, line := range strings.Split(strings.TrimSuffix(s.emu.Render(), "\n"), "\n") {
		if i >= s.emu.Height() {
			break
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[m", i+1, line)
	}
	pos := s.emu.CursorPosition()
	fmt.Fprintf(&b, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	if !s.emu.CursorHidden() {
		b.WriteString("\x1b[?25h")
	}
	return []byte(b.String())
}

// oscFilter drops every OSC string (`ESC ] … BEL` or `ESC ] … ESC \`) that holds a non-ASCII byte before it
// reaches the emulator. The pinned charmbracelet/x/vt (and its main of 2026-11-26) ends an OSC at the first
// UTF-8 byte and prints the rest as text, so Claude Code's window title (`ESC ] 0 ; ✳ Claude Code BEL`, sent
// whenever the cursor sits in the prompt) showed as " Claude Code" typed into the input box, and the box read as
// a draft (found live, docs/design.md-era "Claude Code 2.1.332"). Only a title or a link can carry such text, and
// no client needs either from a repaint; an ASCII OSC (a colour query, a hyperlink) passes through whole. It holds
// an OSC until it ends, across writes, and gives one that never ends to the emulator as it came after oscMax
// bytes.
type oscFilter struct {
	state int    // 0 text, 1 after ESC, 2 inside an OSC string, 3 after ESC inside an OSC string
	held  []byte // the OSC string so far, with its introducer
	wide  bool   // it holds a byte of 0x80 or more
}

const oscMax = 4096

func (f *oscFilter) filter(p []byte) []byte {
	var out []byte
	for _, b := range p {
		switch f.state {
		case 0:
			if b == 0x1b {
				f.state = 1
				f.held = append(f.held[:0], b)
				continue
			}
			out = append(out, b)
		case 1:
			if b == ']' {
				f.state = 2
				f.wide = false
				f.held = append(f.held, b)
				continue
			}
			// Not an OSC: the held ESC and this byte go on (an ESC ESC keeps waiting on the second).
			out = append(out, f.held...)
			f.held = f.held[:0]
			if b == 0x1b {
				f.held = append(f.held, b)
				continue
			}
			f.state = 0
			out = append(out, b)
		case 2, 3:
			f.held = append(f.held, b)
			switch {
			case f.state == 3 && b == '\\', f.state == 2 && b == 0x07:
				if !f.wide {
					out = append(out, f.held...)
				}
				f.reset()
				continue
			case b == 0x1b:
				f.state = 3
			case f.state == 3 && b == ']': // ESC ] cuts the string short and starts the next one
				if !f.wide { // end it properly: the next one is ours to judge, and the emulator must not stay inside this one
					out = append(append(out, f.held[:len(f.held)-2]...), 0x1b, '\\')
				}
				f.state, f.wide = 2, false
				f.held = append(f.held[:0], 0x1b, ']')
				continue
			case f.state == 3: // ESC then something else: the string ends there, as the emulator ends it
				if !f.wide {
					out = append(out, f.held...)
				} else {
					out = append(out, 0x1b, b) // the string goes, the escape sequence that cut it short stays
				}
				f.reset()
				continue
			default:
				f.state = 2
			}
			if b >= 0x80 {
				f.wide = true
			}
			if len(f.held) > oscMax {
				out = append(out, f.held...)
				f.reset()
			}
		}
	}
	return out
}

func (f *oscFilter) reset() { f.state, f.held, f.wide = 0, f.held[:0], false }
