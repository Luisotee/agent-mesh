package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWSStreamConnReadDeadlineDoesNotCloseSocket(t *testing.T) {
	srvDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		c := NewWSConn(ws, nil, nil)
		defer c.Close()

		buf := make([]byte, 64)
		// Simulate net/http's abortPendingRead (past deadline) while keeping the connection alive.
		_ = c.SetReadDeadline(time.Now().Add(-time.Second))
		if _, err := c.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("past deadline read = %v, want os.ErrDeadlineExceeded", err)
		}
		_ = c.SetReadDeadline(time.Time{})

		n, err := c.Read(buf)
		if err != nil {
			t.Errorf("read after clearing deadline: %v", err)
			return
		}
		if _, err := c.Write(bytes.ToUpper(buf[:n])); err != nil {
			t.Errorf("write: %v", err)
		}
		close(srvDone)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	c := NewWSConn(ws, nil, nil)
	defer c.Close()

	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "HELLO" {
		t.Fatalf("got %q, want HELLO", got)
	}
	<-srvDone
}

func TestSpliceBidirectional(t *testing.T) {
	c1, s1 := net.Pipe()
	s2, c2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		Splice(ctx, s1, s2)
		close(done)
	}()

	go func() {
		_, _ = c1.Write([]byte("ping"))
		buf := make([]byte, 4)
		_, _ = io.ReadFull(c1, buf)
		if string(buf) != "pong" {
			t.Errorf("c1 got %q", buf)
		}
		_ = c1.Close()
	}()

	buf := make([]byte, 4)
	if _, err := io.ReadFull(c2, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("c2 got %q, %v", buf, err)
	}
	if _, err := c2.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	<-done
}
