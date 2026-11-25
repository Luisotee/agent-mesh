package dev.agentmesh.android

import com.termux.terminal.TextStyle
import dev.agentmesh.android.term.TermScreen
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** The emulator under the screen, fed real escape sequences. */
class TermScreenTest {
    private fun screen(cols: Int = 20, rows: Int = 5) = TermScreen().also { it.reset(cols, rows) }
    private fun TermScreen.feed(s: String) = write(s.toByteArray())

    @Test fun `no screen until an attach says how big it is`() {
        val s = TermScreen()
        assertNull(s.emulator)
        s.write("ignored".toByteArray())
        assertEquals("", s.visibleText())
    }

    @Test fun `text and line ends land on the rows`() {
        val s = screen()
        s.feed("hello\r\nworld")
        assertEquals("hello", s.rowText(0)); assertEquals("world", s.rowText(1))
        assertEquals("hello\nworld", s.visibleText())
    }

    @Test fun `colours and styles are kept per cell`() {
        val s = screen()
        s.feed("\u001b[31;1mred\u001b[0m ok")
        val line = s.emulator!!.screen.allocateFullLineIfNecessary(s.emulator!!.screen.externalToInternalRow(0))
        assertEquals(1, TextStyle.decodeForeColor(line.getStyle(0)))
        assertTrue(TextStyle.decodeEffect(line.getStyle(0)) and TextStyle.CHARACTER_ATTRIBUTE_BOLD != 0)
        assertEquals(TextStyle.COLOR_INDEX_FOREGROUND, TextStyle.decodeForeColor(line.getStyle(5)))
        // True colour passes through as 24-bit.
        s.feed("\r\n\u001b[38;2;10;20;30mT")
        val l2 = s.emulator!!.screen.allocateFullLineIfNecessary(s.emulator!!.screen.externalToInternalRow(1))
        assertEquals(0xff0a141e.toInt(), TextStyle.decodeForeColor(l2.getStyle(0)))
    }

    @Test fun `wide characters take two columns`() {
        val s = screen()
        s.feed("日本x")
        assertEquals("日本x", s.rowText(0))
        assertEquals(5, s.emulator!!.cursorCol)
    }

    @Test fun `lines pushed off the top are kept as scrollback`() {
        val s = screen(rows = 3)
        s.feed((1..10).joinToString("\r\n") { "line $it" })
        assertEquals("line 10", s.rowText(2))
        assertEquals("line 7", s.rowText(-1)) // the line just above the screen
        assertEquals(7, s.scrollbackRows)
    }

    @Test fun `a reader of older lines stays on them as new ones arrive`() {
        val s = screen(rows = 3)
        s.feed((1..10).joinToString("\r\n") { "line $it" })
        s.scrollBack = 2
        assertEquals("line 6", s.visibleText().lines().first())
        s.feed("\r\nline 11\r\nline 12")
        assertEquals("the same lines are still on screen", "line 6", s.visibleText().lines().first())
        assertEquals(4, s.scrollBack)
        // Following the output (0) stays at the bottom.
        s.scrollBack = 0
        s.feed("\r\nline 13")
        assertEquals("line 13", s.visibleText().lines().last())
    }

    @Test fun `terminal queries are not answered`() {
        // Device attributes, cursor position and a window report: the emulator would reply; a viewer must not.
        val s = screen()
        s.feed("\u001b[c\u001b[6n\u001b[18t\u001b[>c")
        assertEquals("", s.visibleText())
    }

    @Test fun `a reset forgets the old screen`() {
        val s = screen()
        s.feed("old")
        s.reset(30, 8)
        assertEquals(30, s.cols); assertEquals(8, s.rows); assertEquals("", s.visibleText())
    }
}
