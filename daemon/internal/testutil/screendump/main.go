// Command screendump prints what a terminal of a terminal host shows (its repaint) and what the input box
// profile of a CLI reads from it. A debugging aid for the live checks (README.md, "Live checks"): run by hand.
//
//	screendump --term <host dir> --id <terminal id> [--profile claude|agy|copilot|opencode] [--raw]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/inputbox"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;:?]*[a-zA-Z]`)

func main() {
	dir := flag.String("term", "", "the terminal host's dir (<state>/term)")
	id := flag.String("id", "", "terminal id")
	profile := flag.String("profile", "claude", "input box profile: claude, agy, copilot, opencode")
	raw := flag.Bool("raw", false, "print the raw repaint with escapes")
	replay := flag.Bool("replay", false, "print the raw output stream from the start of the host's ring (with --raw)")
	write := flag.String("write", "", "type this into the terminal first (Go string escapes: \\r, \\x15), then wait a second")
	cols := flag.Int("cols", 0, "attach at this width (resizes the terminal)")
	rows := flag.Int("rows", 0, "attach at this height")
	flag.Parse()
	if *dir == "" || *id == "" {
		flag.Usage()
		os.Exit(2)
	}
	c, err := termclient.New(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if *write != "" {
		text, err := strconv.Unquote(`"` + *write + `"`)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, err := c.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: *id, Data: []byte(text)})); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}
	s, err := termclient.Attach(ctx, c.HTTP, client.BaseURL+"/term/"+*id, c.Token,
		&termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT, Cols: uint32(*cols), Rows: uint32(*rows)})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer s.Close()
	var buf []byte
	if *replay {
		var epoch uint64
		for {
			f, err := s.Recv(ctx)
			if err != nil {
				break
			}
			if at := f.GetAttached(); at != nil {
				epoch = at.Epoch
				break
			}
		}
		s.Close()
		s, err = termclient.Attach(ctx, c.HTTP, client.BaseURL+"/term/"+*id, c.Token,
			&termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_EXACT, Epoch: epoch, Offset: 0})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer s.Close()
	}
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			break
		}
		if sc := f.GetScreen(); sc != nil {
			buf = append(buf, sc.Data...)
		}
		if o := f.GetOutput(); o != nil {
			buf = append(buf, o.Data...)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	if *raw {
		fmt.Printf("%q\n", buf)
	} else {
		fmt.Println(strings.TrimRight(sgr.ReplaceAllString(string(buf), ""), "\n "))
	}
	p := map[string]inputbox.Profile{"claude": inputbox.Claude, "agy": inputbox.Agy, "copilot": inputbox.Copilot,
		"opencode": inputbox.OpenCode}[*profile]
	r := inputbox.Read(buf, p, "")
	fmt.Printf("--- inputbox(%s): %s %q\n", *profile, r.State, r.Text)
}
