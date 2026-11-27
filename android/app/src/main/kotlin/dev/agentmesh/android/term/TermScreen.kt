package dev.agentmesh.android.term

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.setValue
import com.termux.terminal.TerminalEmulator
import com.termux.terminal.TerminalOutput
import com.termux.terminal.TerminalSessionClient

/**
 * A viewer's terminal: it never answers. The emulator writes its replies to terminal queries (device attributes,
 * cursor position) here, and this drops them, because the host answers those itself while nobody who can type is
 * attached, and a second answer would reach the program as typed input. A phone that types sends only what the user
 * typed or pasted, never these replies (the host has then left the answering to the typing client: the typing is the
 * user's, the queries wait for a client that answers them).
 */
internal class ReadOnlyOutput : TerminalOutput() {
    /**
     * Set only while [TermScreen.pasteBytes] asks the emulator to encode a paste (bracketed or not, as the program set it):
     * what it writes then is the paste, and is taken. Everything else it writes is a reply to a query, and is dropped.
     */
    var capture: java.io.ByteArrayOutputStream? = null

    override fun write(data: ByteArray?, offset: Int, count: Int) {
        if (data != null) capture?.write(data, offset, count)
    }
    override fun titleChanged(oldTitle: String?, newTitle: String?) {}
    override fun onCopyTextToClipboard(text: String?) {}
    override fun onPasteTextFromClipboard() {}
    override fun onBell() {}
    override fun onColorsChanged() {}
}

private object QuietClient : TerminalSessionClient {
    override fun onTerminalCursorStateChange(state: Boolean) {}
    override fun getTerminalCursorStyle(): Int? = null
    override fun logError(tag: String?, message: String?) {}
    override fun logWarn(tag: String?, message: String?) {}
    override fun logInfo(tag: String?, message: String?) {}
    override fun logDebug(tag: String?, message: String?) {}
    override fun logVerbose(tag: String?, message: String?) {}
    override fun logStackTraceWithMessage(tag: String?, message: String?, e: Exception?) {}
    override fun logStackTrace(tag: String?, e: Exception?) {}
}

/** Lines kept above the screen for scrolling back. */
const val SCROLLBACK_ROWS = 2_000

/**
 * What the terminal looks like now: a Termux emulator fed the bytes of the remote terminal. Everything here runs on
 * the main thread (the emulator isn't thread-safe, and the view draws from it); [version] changes whenever the
 * screen did, so the view redraws.
 */
class TermScreen {
    internal val output = ReadOnlyOutput()

    var emulator: TerminalEmulator? = null
        private set

    /** Bumped on every change: the view reads it, so it is redrawn. */
    var version by mutableIntStateOf(0)
        private set

    /** Rows scrolled back from the live screen (0 follows the output). */
    var scrollBack by mutableIntStateOf(0)

    val cols: Int get() = emulator?.mColumns ?: 0
    val rows: Int get() = emulator?.mRows ?: 0
    val scrollbackRows: Int get() = emulator?.screen?.activeTranscriptRows ?: 0

    /** How many times the screen was made again: a selection, which names cells of one screen, is only good for the count it was made at. */
    var resets by mutableIntStateOf(0)
        private set

    /** A new screen of this size (an attach that repaints): nothing of the old one is kept. */
    fun reset(cols: Int, rows: Int) {
        emulator = TerminalEmulator(output, cols.coerceAtLeast(2), rows.coerceAtLeast(1), 1, 1, SCROLLBACK_ROWS, QuietClient)
        scrollBack = 0
        resets++
        version++
    }

    /** Output bytes. While the user reads older lines, the view stays on them as new ones push the screen up. */
    fun write(data: ByteArray) {
        val emu = emulator ?: return
        val buffer = emu.screen
        val before = buffer.historyScrolls
        emu.append(data, data.size)
        // Lines pushed into the history, counted even once it is full and drops its oldest (a scroll inside horizontal margins pushes none).
        if (scrollBack > 0) {
            val pushed = if (emu.screen === buffer) emu.screen.historyScrolls - before else 0
            scrollBack = (scrollBack + pushed).coerceIn(0, emu.screen.activeTranscriptRows)
        }
        version++
    }

    /** The text of row [row] (0 is the top of the live screen, negative rows are scrollback), trailing blanks cut. */
    fun rowText(row: Int): String {
        val emu = emulator ?: return ""
        val screen = emu.screen
        if (row < -screen.activeTranscriptRows || row >= emu.mRows) return ""
        return screen.getSelectedText(0, row, emu.mColumns - 1, row).trimEnd()
    }

    /** The lines on screen, for accessibility and tests. */
    fun visibleText(): String = (0 until rows).joinToString("\n") { rowText(it - scrollBack) }.trimEnd()
}

/** Rows scrolled out of the live screen so far: a row's place in the whole history is its row now plus this. */
val TermScreen.historyScrolls: Long get() = (emulator?.screen?.historyScrolls ?: 0).toLong()

/** Whether the program set the cursor keys to application mode (DECCKM): arrows are then `ESC O A`, not `ESC [ A`. */
val TermScreen.cursorKeysApplication: Boolean get() = emulator?.isCursorKeysApplicationMode ?: false

/** A row of the live screen or its history ([ext]: 0 the top of the live screen, negative into the history) as its place in the whole history. */
fun TermScreen.absoluteRow(ext: Int): Long = ext + historyScrolls

/** The row's place now ([abs] is as [absoluteRow] gave it): above what is kept it has scrolled out. */
fun TermScreen.externalRow(abs: Long): Int = (abs - historyScrolls).coerceIn(Int.MIN_VALUE.toLong(), Int.MAX_VALUE.toLong()).toInt()

/**
 * The text of a selection, rows that have scrolled out of the history left off the top. Columns are kept inside the screen
 * (a selection made before the screen got narrower must not read past its rows), and each end takes the whole of a wide
 * character it lands on, as the highlight does: the emulator's own selection takes a wide character only from its second column.
 */
fun TermScreen.selectedText(sel: Selection): String {
    val emu = emulator ?: return ""
    val s = sel.start
    val e = sel.end
    val minRow = -emu.screen.activeTranscriptRows
    val maxRow = emu.mRows - 1
    var r1 = externalRow(s.row)
    var r2 = externalRow(e.row)
    if (r2 < minRow || r1 > maxRow) return ""
    val last = emu.mColumns - 1
    var c1 = s.col.coerceIn(0, last)
    var c2 = e.col.coerceIn(0, last)
    if (r1 < minRow) { r1 = minRow; c1 = 0 }
    if (r2 > maxRow) { r2 = maxRow; c2 = last }
    c1 = glyphRange(r1, c1).first
    c2 = glyphRange(r2, c2).last.coerceAtMost(last)
    return emu.screen.getSelectedText(c1, r1, c2, r2).trimEnd()
}

/**
 * [text] as the bytes to type for a paste: control characters dropped, newlines as carriage returns, and, when the program
 * asked for bracketed paste (DECSET 2004), between its start and end marks; the emulator does that, so it stays the program's own rule.
 */
fun TermScreen.pasteBytes(text: String): ByteArray {
    val emu = emulator ?: return ByteArray(0)
    val out = java.io.ByteArrayOutputStream()
    output.capture = out
    try { emu.paste(text) } finally { output.capture = null }
    return out.toByteArray()
}

/** One character of a row as the emulator holds it: its first column, its width in columns, and whether it is blank. */
private class Glyph(val start: Int, val width: Int, val blank: Boolean) {
    val end get() = start + width - 1
}

/**
 * The characters of row [ext] by their cells: a wide character (CJK, an emoji) takes two columns and a surrogate pair two
 * chars of the row's text, so a column is not an index into the text. Null for a row that isn't there.
 */
private fun TermScreen.glyphs(ext: Int): List<Glyph>? {
    val emu = emulator ?: return null
    val buf = emu.screen
    if (ext < -buf.activeTranscriptRows || ext >= emu.mRows) return null
    val line = buf.allocateFullLineIfNecessary(buf.externalToInternalRow(ext))
    val text = line.mText
    val used = line.spaceUsed
    val out = ArrayList<Glyph>()
    var c = 0
    var i = 0
    while (c < emu.mColumns) {
        var cp = ' '.code
        var chars = 0
        if (i < used) {
            val ch = text[i]
            if (Character.isHighSurrogate(ch) && i + 1 < used) { cp = Character.toCodePoint(ch, text[i + 1]); chars = 2 } else { cp = ch.code; chars = 1 }
        }
        val w = if (chars == 0) 1 else maxOf(com.termux.terminal.WcWidth.width(cp), 0)
        if (w > 0) {
            out.add(Glyph(c, w, Character.isWhitespace(cp) || chars == 0))
            c += w
        }
        i += chars
    }
    return out
}

/** The columns of the character that [col] on row [ext] is in: both halves of a wide one, else the column itself. */
fun TermScreen.glyphRange(ext: Int, col: Int): IntRange {
    val g = glyphs(ext)?.lastOrNull { it.start <= col }
    return if (g == null || col > g.end) col..col else g.start..g.end
}

/**
 * The columns of the word at [col] on row [ext] of the screen (a run of non-blank characters). A blank cell, or a column
 * past the line, is a one-cell range.
 */
fun TermScreen.wordRange(ext: Int, col: Int): IntRange {
    val gs = glyphs(ext) ?: return col..col
    val at = gs.indexOfLast { it.start <= col }
    if (at < 0 || col > gs[at].end || gs[at].blank) return col..col
    var a = at
    var b = at
    while (a > 0 && !gs[a - 1].blank) a--
    while (b < gs.size - 1 && !gs[b + 1].blank) b++
    return gs[a].start..gs[b].end
}
