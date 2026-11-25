package termhost

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
)

func TestRingKeepsTheNewestBytes(t *testing.T) {
	r := NewRing(8)
	r.Append([]byte("abcde"))
	if got, ok := r.Since(2); !ok || string(got) != "cde" {
		t.Errorf("Since(2) = %q, %v", got, ok)
	}
	r.Append([]byte("fghij"))
	if r.Start() != 2 || r.End() != 10 {
		t.Errorf("start %d end %d", r.Start(), r.End())
	}
	if _, ok := r.Since(1); ok {
		t.Error("an offset that left the ring was served")
	}
	if got, _ := r.Since(2); string(got) != "cdefghij" {
		t.Errorf("Since(2) = %q", got)
	}
	r.Append([]byte("0123456789"))
	if got, _ := r.Since(12); string(got) != "23456789" || r.Start() != 12 {
		t.Errorf("after a big append: %q start %d", got, r.Start())
	}
	if _, ok := r.Since(21); ok {
		t.Error("a future offset was served")
	}
	if got, ok := r.Since(20); !ok || len(got) != 0 {
		t.Errorf("Since(end) = %q, %v", got, ok)
	}
}

func TestRepaintShowsTheScreenCursorAndModes(t *testing.T) {
	replies := make(chan []byte, 4)
	s := NewScreen(20, 4, func(b []byte) { replies <- b })
	defer s.Close()
	s.Write([]byte("hello \x1b[31mred\x1b[0m\r\nline2\x1b[?2004h\x1b[?1h"))
	got := string(s.Repaint())
	for _, want := range []string{"\x1b[?1h", "\x1b[?2004h", "hello", "red", "line2", "\x1b[2;6H", "\x1b[?25h"} {
		if !strings.Contains(got, want) {
			t.Errorf("repaint lacks %q: %q", want, got)
		}
	}
	s.Write([]byte("\x1b[?2004l\x1b[?1049h\x1b[?25l"))
	got = string(s.Repaint())
	if strings.Contains(got, "\x1b[?2004h") || !strings.HasPrefix(got, "\x1b[?1049h") || strings.Contains(got, "\x1b[?25h") {
		t.Errorf("modes not followed: %q", got)
	}
	s.Write([]byte("\x1b[6n"))
	select {
	case r := <-replies:
		if !strings.HasPrefix(string(r), "\x1b[") || !strings.HasSuffix(string(r), "R") {
			t.Errorf("cursor position report %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Error("no cursor position report")
	}
}

func TestASlowClientIsDroppedAndOutputNeverWaits(t *testing.T) {
	h := &Host{log: slog.New(slog.NewTextHandler(io.Discard, nil)), watchers: map[chan *termv1.WatchTerminalsResponse]struct{}{}}
	term := &Terminal{h: h, ring: NewRing(1 << 20), screen: NewScreen(80, 24, nil), epoch: 1, clients: map[*client]struct{}{}}
	defer term.screen.Close()
	slow := &client{out: make(chan *termv1.TermFrame, 4), gone: make(chan struct{})}
	term.clients[slow] = struct{}{}
	for i := 0; i < 10; i++ {
		term.output(bytes.Repeat([]byte("x"), 100))
	}
	select {
	case <-slow.gone:
	default:
		t.Fatal("slow client not dropped")
	}
	if slow.reason != "slow" || len(term.clients) != 0 || term.ring.End() != 1000 {
		t.Errorf("reason %q, %d clients, end %d", slow.reason, len(term.clients), term.ring.End())
	}
}
