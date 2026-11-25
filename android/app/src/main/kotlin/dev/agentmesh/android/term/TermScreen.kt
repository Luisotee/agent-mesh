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
 * attached, and a second answer would reach the program as typed input.
 */
private object ReadOnlyOutput : TerminalOutput() {
    override fun write(data: ByteArray?, offset: Int, count: Int) {}
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

    /** A new screen of this size (an attach that repaints): nothing of the old one is kept. */
    fun reset(cols: Int, rows: Int) {
        emulator = TerminalEmulator(ReadOnlyOutput, cols.coerceAtLeast(2), rows.coerceAtLeast(1), 1, 1, SCROLLBACK_ROWS, QuietClient)
        scrollBack = 0
        version++
    }

    /** Output bytes. While the user reads older lines, the view stays on them as new ones push the screen up. */
    fun write(data: ByteArray) {
        val emu = emulator ?: return
        val before = emu.screen.activeTranscriptRows
        emu.append(data, data.size)
        if (scrollBack > 0) scrollBack = (scrollBack + emu.screen.activeTranscriptRows - before).coerceIn(0, emu.screen.activeTranscriptRows)
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
