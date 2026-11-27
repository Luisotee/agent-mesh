package termhost

import "regexp"

// What a terminal emulator sends on its own, not from a key pressed: focus reports (`ESC [ I`, `ESC [ O`, sent on
// every focus change once the program turned mode 1004 on, as Claude Code does), a cursor position report
// (`ESC [ row ; col R`, or `ESC [ ? row ; col R`), a private mode's state (DECRPM, `ESC [ ? mode ; value $ y`), the
// kitty keyboard flags (`ESC [ ? flags u`), device attributes (`ESC [ ? … c`, `ESC [ > … c`), a status report (`ESC [ 0 n`), and OSC
// replies such as a colour query's (`ESC ] 11 ; rgb:… BEL` or `ST`). xterm.js sends them while a user only looks at a
// terminal (clicking into the window, switching tabs), so they must not count as someone typing. A cursor position
// report has the same bytes as a modified F3 (`ESC [ 1 ; 2 R`, Shift+F3): such a key still reaches the program, it
// only doesn't count as typing.
var report = regexp.MustCompile(`^(?:\x1b\[[IO]|\x1b\[\??\d+;\d+R|\x1b\[[?>][\d;]*c|\x1b\[\d+n|\x1b\[\?[\d;]*\$y|\x1b\[\?\d*u|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\))+$`)

// OnlyReports reports whether a client's input is nothing but terminal replies: no key a person pressed.
func OnlyReports(data []byte) bool {
	return len(data) > 0 && report.Match(data)
}
