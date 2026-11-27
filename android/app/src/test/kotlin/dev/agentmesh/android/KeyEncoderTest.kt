package dev.agentmesh.android

import dev.agentmesh.android.term.CellPos
import dev.agentmesh.android.term.ImeBuffer
import dev.agentmesh.android.term.Key
import dev.agentmesh.android.term.KeyEncoder
import dev.agentmesh.android.term.Selection
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** What the key row and the soft keyboard send to the program, and how a selection is read: all pure. */
class KeyEncoderTest {
    private fun s(b: ByteArray) = b.toString(Charsets.UTF_8)

    @Test fun `the plain keys are what a terminal sends`() {
        assertEquals("\u001B", s(KeyEncoder.key(Key.Esc)))
        assertEquals("\t", s(KeyEncoder.key(Key.Tab)))
        assertEquals("\u001B[Z", s(KeyEncoder.key(Key.ShiftTab)))
        assertEquals("\r", s(KeyEncoder.key(Key.Enter)))
        assertEquals("\u007f", s(KeyEncoder.key(Key.Backspace)))
        assertEquals("\u001B[3~", s(KeyEncoder.key(Key.Delete)))
        assertEquals("\u001B[5~", s(KeyEncoder.key(Key.PageUp))); assertEquals("\u001B[6~", s(KeyEncoder.key(Key.PageDown)))
    }

    @Test fun `the cursor keys follow the program's application mode, and Ctrl or Alt make a modified sequence`() {
        assertEquals("\u001B[A", s(KeyEncoder.key(Key.Up)))
        assertEquals("\u001BOA", s(KeyEncoder.key(Key.Up, cursorApplication = true)))
        assertEquals("\u001B[H", s(KeyEncoder.key(Key.Home))); assertEquals("\u001BOF", s(KeyEncoder.key(Key.End, cursorApplication = true)))
        // xterm's modifier parameter: Alt 3, Ctrl 5, both 7 (the modified form is the same in either mode).
        assertEquals("\u001B[1;5C", s(KeyEncoder.key(Key.Right, ctrl = true)))
        assertEquals("\u001B[1;3D", s(KeyEncoder.key(Key.Left, alt = true, cursorApplication = true)))
        assertEquals("\u001B[1;7B", s(KeyEncoder.key(Key.Down, ctrl = true, alt = true)))
        assertEquals("\u001B[3;5~", s(KeyEncoder.key(Key.Delete, ctrl = true)))
        assertEquals("\u001B\r", s(KeyEncoder.key(Key.Enter, alt = true)))
    }

    @Test fun `Ctrl makes a control code of a letter, Alt an escape before it, and text is UTF-8`() {
        assertArrayEquals(byteArrayOf(3), KeyEncoder.text("c", ctrl = true))
        assertArrayEquals(byteArrayOf(3), KeyEncoder.text("C", ctrl = true))
        assertArrayEquals(byteArrayOf(27), KeyEncoder.text("[", ctrl = true))
        assertArrayEquals(byteArrayOf(0x7f), KeyEncoder.text("?", ctrl = true))
        assertArrayEquals(byteArrayOf(0), KeyEncoder.text(" ", ctrl = true))
        // A character with no control code is sent as it is.
        assertEquals("5", s(KeyEncoder.text("5", ctrl = true)))
        assertEquals("\u001Bb", s(KeyEncoder.text("b", alt = true)))
        assertEquals("é😀", s(KeyEncoder.text("é😀")))
        assertEquals("\u001B\u0004", s(KeyEncoder.text("d", ctrl = true, alt = true)))
    }

    @Test fun `the soft keyboard's changes of the hidden field are typing, Backspace and Enter`() {
        val z = ImeBuffer.SENTINEL
        assertEquals("ls", s(ImeBuffer.change(z, z + "ls").first))
        assertEquals("\u007f", s(ImeBuffer.change(z, "").first))
        assertEquals("a\rb", s(ImeBuffer.change(z, z + "a\nb").first))
        assertEquals(z, ImeBuffer.change(z, z + "x").second)
        // A word replaced by autocorrect: deletions, then the new text.
        assertEquals("\u007f\u007fok", s(ImeBuffer.change(z + "ab", z + "ok").first))
        // Sticky Ctrl applies to what is typed.
        assertArrayEquals(byteArrayOf(4), ImeBuffer.change(z, z + "d", ctrl = true).first)
        assertEquals(0, ImeBuffer.change(z, z).first.size)
    }

    @Test fun `a selection runs from the first cell to the last, either way`() {
        val sel = Selection(CellPos(5, 10), CellPos(2, 12))
        assertEquals(CellPos(5, 10), sel.start); assertEquals(CellPos(2, 12), sel.end)
        // First row from its column, whole rows between, last row up to its column.
        assertTrue(sel.contains(5, 10, 80)); assertFalse(sel.contains(4, 10, 80))
        assertTrue(sel.contains(0, 11, 80)); assertTrue(sel.contains(79, 11, 80))
        assertTrue(sel.contains(2, 12, 80)); assertFalse(sel.contains(3, 12, 80))
        assertFalse(sel.contains(5, 9, 80)); assertFalse(sel.contains(0, 13, 80))
        val back = Selection(CellPos(2, 12), CellPos(5, 10))
        assertEquals(sel.start, back.start); assertEquals(sel.end, back.end)
    }

    @Test fun `a long press takes the word under the finger, a blank takes itself`() {
        val line = "git commit -m fix"
        assertEquals(4..9, Selection.wordAt(line, 6))
        assertEquals(0..2, Selection.wordAt(line, 0))
        assertEquals(14..16, Selection.wordAt(line, 16))
        assertEquals(3..3, Selection.wordAt(line, 3))
        assertEquals(40..40, Selection.wordAt(line, 40))
    }

    @Test fun `a point is the cell under it, clamped to the screen, for the row it shows`() {
        // Cells 10 by 20 px, panned 30 px left, the top row being history row 100 (scrolled back).
        assertEquals(CellPos(10, 102), Selection.cellAt(75f, 45f, 10f, 20f, -30f, 0f, 100, 80, 24))
        assertEquals(CellPos(0, 100), Selection.cellAt(-500f, -10f, 10f, 20f, 0f, 0f, 100, 80, 24))
        assertEquals(CellPos(79, 123), Selection.cellAt(5000f, 5000f, 10f, 20f, 0f, 0f, 100, 80, 24))
        // The top rows cut off when the host's terminal is taller than the view (a negative shift).
        assertEquals(CellPos(0, 105), Selection.cellAt(0f, 0f, 10f, 20f, 0f, -100f, 100, 80, 24))
    }
}
