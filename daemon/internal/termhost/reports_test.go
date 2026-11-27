package termhost

import "testing"

func TestTerminalRepliesAreNotTyping(t *testing.T) {
	for _, in := range []string{
		"\x1b[I", "\x1b[O", "\x1b[O\x1b[I", // focus out and in
		"\x1b[12;40R",                  // cursor position
		"\x1b[?1;2c", "\x1b[>0;276;0c", // device attributes
		"\x1b[0n",                                                            // status
		"\x1b]11;rgb:2828/2a2a/3636\x07", "\x1b]10;rgb:f8f8/f8f8/f2f2\x1b\\", // colour queries
	} {
		if !OnlyReports([]byte(in)) {
			t.Errorf("%q counted as typing", in)
		}
	}
	for _, in := range []string{
		"a", "hello", "\r", "\x7f", "\x03", "\x15", // keys
		"\x1b",                // Esc pressed
		"\x1b[A", "\x1b[1;5C", // arrows, Ctrl+Right
		"\x1b[Ia",                // a focus report with a key after it
		"x\x1b[O",                // a key with a focus report after it
		"\x1b[200~text\x1b[201~", // a paste
		"",
	} {
		if OnlyReports([]byte(in)) {
			t.Errorf("%q not counted as typing", in)
		}
	}
}
