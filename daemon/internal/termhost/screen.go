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
	1:    true, // application cursor keys
	1000: true, 1002: true, 1003: true, 1005: true, 1006: true, 1015: true, // mouse reporting
	1004: true, // focus events
	2004: true, // bracketed paste
}

type vtScreen struct {
	emu     *vt.Emulator
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

func (s *vtScreen) Write(p []byte)        { s.emu.Write(p) }
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
