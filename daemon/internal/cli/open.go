package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/term"
	"github.com/coder/websocket"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

const detachByte = 0x1d // Ctrl-]

var errDetached = errors.New("detached")

func runOpen(ctx context.Context, conn *client.Conn, res *v1.OpenAgentResponse, env Env) error {
	switch res.Mode {
	case v1.OpenMode_OPEN_MODE_ATTACH:
		return attachOpen(ctx, conn, res, env)
	case v1.OpenMode_OPEN_MODE_EXEC:
		return execOpen(res, env)
	default:
		return fmt.Errorf("unexpected open mode %v", res.Mode)
	}
}

func openAgentLabel(res *v1.OpenAgentResponse) string {
	if name := res.GetAgent().GetName(); name != "" {
		return name
	}
	return res.GetAgent().GetId()
}

func execOpen(res *v1.OpenAgentResponse, env Env) error {
	if len(res.Argv) == 0 {
		return errors.New("daemon returned an empty command")
	}
	name := openAgentLabel(res)
	if res.Branch != "" {
		fmt.Fprintf(env.Stderr, "Reopening %s (%s) on branch %s\n", name, res.GetAgent().GetKind(), res.Branch)
	} else {
		fmt.Fprintf(env.Stderr, "Reopening %s (%s)\n", name, res.GetAgent().GetKind())
	}
	for _, note := range res.Notes {
		fmt.Fprintln(env.Stderr, note)
	}
	environFn := env.Environ
	if environFn == nil {
		environFn = os.Environ
	}
	childEnv := buildStandaloneEnv(environFn(), res.Env)
	execFn := env.Exec
	if execFn == nil {
		execFn = defaultExec
	}
	return execFn(res.Argv, childEnv, res.Cwd)
}

func buildStandaloneEnv(hostEnv []string, overrides map[string]string) []string {
	drop := make(map[string]bool, len(claude.SessionVars)+len(agy.SessionVars))
	for _, k := range claude.SessionVars {
		drop[k] = true
	}
	for _, k := range agy.SessionVars {
		drop[k] = true
	}
	merged := map[string]string{}
	for _, kv := range hostEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "AGENT_MESH_") || strings.HasPrefix(k, "MESH_") || drop[k] {
			continue
		}
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func defaultExec(argv, env []string, dir string) error {
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			return err
		}
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(bin, argv, env)
}

func attachOpen(ctx context.Context, conn *client.Conn, res *v1.OpenAgentResponse, env Env) error {
	inFile, inOK := env.Stdin.(*os.File)
	outFile, outOK := env.Stdout.(*os.File)
	if !inOK || !outOK || !term.IsTerminal(inFile.Fd()) {
		return fmt.Errorf("cannot attach to %s: stdin/stdout must be a terminal", openAgentLabel(res))
	}
	cols, rows, err := term.GetSize(outFile.Fd())
	if err != nil || cols <= 0 || rows <= 0 {
		if c2, r2, err2 := term.GetSize(inFile.Fd()); err2 == nil && c2 > 0 && r2 > 0 {
			cols, rows = c2, r2
		} else {
			cols, rows = 80, 24
		}
	}

	name := openAgentLabel(res)
	fmt.Fprintf(env.Stderr, "Attached to %s (Ctrl-] to detach)\n", name)

	attachCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := termclient.Attach(attachCtx, conn.HTTP, client.BaseURL+res.TerminalPath, conn.Token, &termv1.Attach{
		Mode:   termv1.AttachMode_ATTACH_MODE_REPAINT,
		Cols:   uint32(cols),
		Rows:   uint32(rows),
		Redraw: true,
	})
	if err != nil {
		return err
	}
	defer stream.Close()

	oldState, err := term.MakeRaw(inFile.Fd())
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			_ = term.Restore(inFile.Fd(), oldState)
		}
	}
	defer restore()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-attachCtx.Done():
				return
			case <-winch:
				c, r, gerr := term.GetSize(outFile.Fd())
				if gerr != nil || c <= 0 || r <= 0 {
					continue
				}
				_ = stream.Send(attachCtx, &termv1.TermFrame{
					Frame: &termv1.TermFrame_Resize{Resize: &termv1.Resize{Cols: uint32(c), Rows: uint32(r)}},
				})
			}
		}
	}()

	errCh := make(chan error, 2)

	// Pump remote terminal output to stdout.
	go func() {
		for {
			f, rerr := stream.Recv(attachCtx)
			if rerr != nil {
				if status := websocket.CloseStatus(rerr); status == websocket.StatusNormalClosure {
					errCh <- nil
					return
				}
				errCh <- rerr
				return
			}
			switch v := f.Frame.(type) {
			case *termv1.TermFrame_Output:
				if _, werr := outFile.Write(v.Output.Data); werr != nil {
					errCh <- werr
					return
				}
			case *termv1.TermFrame_Screen:
				if _, werr := outFile.Write(v.Screen.Data); werr != nil {
					errCh <- werr
					return
				}
			case *termv1.TermFrame_ReplayDone:
				if serr := stream.Ready(attachCtx); serr != nil {
					errCh <- serr
					return
				}
			case *termv1.TermFrame_Exited:
				restore()
				fmt.Fprintf(env.Stderr, "\n[%s exited (%d)]\n", name, v.Exited.ExitCode)
				errCh <- nil
				return
			case *termv1.TermFrame_Closing:
				restore()
				if v.Closing.Reason != "" {
					fmt.Fprintf(env.Stderr, "\n[%s closed: %s]\n", name, v.Closing.Reason)
				} else {
					fmt.Fprintf(env.Stderr, "\n[%s closed]\n", name)
				}
				errCh <- nil
				return
			}
		}
	}()

	// Pump stdin to remote terminal input until Ctrl-] (0x1d).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := inFile.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if idx := bytes.IndexByte(chunk, detachByte); idx >= 0 {
					if idx > 0 {
						_ = stream.Input(attachCtx, chunk[:idx])
					}
					errCh <- errDetached
					return
				}
				if werr := stream.Input(attachCtx, chunk); werr != nil {
					errCh <- werr
					return
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					errCh <- errDetached
				} else {
					errCh <- rerr
				}
				return
			}
		}
	}()

	err = <-errCh
	cancel()
	_ = stream.Close()
	restore()
	if errors.Is(err, errDetached) {
		fmt.Fprintln(env.Stderr, "\n[detached]")
		return nil
	}
	return err
}
