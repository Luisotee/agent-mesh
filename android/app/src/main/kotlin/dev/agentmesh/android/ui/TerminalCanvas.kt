package dev.agentmesh.android.ui

import android.graphics.Paint
import android.graphics.Typeface
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.gestures.detectTransformGestures
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.graphics.nativeCanvas
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import com.termux.terminal.TerminalEmulator
import com.termux.terminal.TextStyle
import com.termux.terminal.WcWidth
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.theme.TerminalColors
import kotlin.math.ceil
import kotlin.math.max
import kotlin.math.min

/** The terminal's colour for a Termux colour value: a 24-bit colour, or an index into the 256-colour palette and the three specials. */
fun terminalColor(value: Int, emu: TerminalEmulator, theme: TerminalColors): Color {
    if ((value.toLong() and 0xff000000L) == 0xff000000L) return Color(value)
    return when {
        value == TextStyle.COLOR_INDEX_FOREGROUND -> theme.foreground
        value == TextStyle.COLOR_INDEX_BACKGROUND -> theme.background
        value == TextStyle.COLOR_INDEX_CURSOR -> theme.cursor
        value in 0..15 -> theme.ansi[value]
        value in 16 until emu.mColors.mCurrentColors.size -> Color(emu.mColors.mCurrentColors[value])
        else -> theme.foreground
    }
}

/** Paints and cell metrics for one text size, made once (a draw per output frame must not allocate them). */
private class TermPaints(val px: Float) {
    val regular = paintOf(px)
    val bold = paintOf(px, bold = true)
    val cell = cellAt(px)
    val chars = CharArray(2)
}

/**
 * How far to move the rows up so the bottom of the screen (the cursor, the prompt) is in view when the host's
 * terminal is taller than the canvas: the top rows are cut instead. 0 when it all fits.
 */
fun bottomShift(heightPx: Float, rows: Int, cellH: Float): Float = min(0f, heightPx - rows * cellH)

private fun paintOf(sizePx: Float, bold: Boolean = false) = Paint(Paint.ANTI_ALIAS_FLAG).apply {
    typeface = if (bold) Typeface.create(Typeface.MONOSPACE, Typeface.BOLD) else Typeface.MONOSPACE
    textSize = sizePx
}

/** A cell's size in pixels at a text size: the monospace advance and the line height. */
class Cell(val w: Float, val h: Float, val baseline: Float)

fun cellAt(sizePx: Float): Cell {
    val p = paintOf(sizePx)
    val m = p.fontMetrics
    return Cell(p.measureText("M"), ceil(m.descent - m.ascent), -m.ascent)
}

private const val MIN_SP = 5f
private const val MAX_SP = 28f
private const val READABLE_SP = 8f

/**
 * Draws a [TermScreen]: each row of the emulator's buffer, cell by cell, in the theme's 16 colours plus the 256 and
 * true colours of the program. Read only: a drag scrolls the history (the view stays on the lines being read as
 * new ones arrive), a pinch zooms and pans, a double tap fits the host's width to the screen. The host's size is
 * whatever the desktop (or the agent's last client) made it; a phone shows it small or scrolls sideways, and never
 * resizes it.
 */
@Composable
fun TerminalCanvas(screen: TermScreen, theme: TerminalColors, modifier: Modifier = Modifier) {
    // 0 fits the host's width to the screen; a pinch sets a size of the user's own.
    var userSp by remember { mutableFloatStateOf(0f) }
    var panX by remember { mutableFloatStateOf(0f) }
    var dragPx by remember { mutableFloatStateOf(0f) }
    // The size last drawn (what a pinch scales) and its pixels per sp: kept outside the state, written while drawing.
    val drawn = remember { floatArrayOf(READABLE_SP, 1f) }
    val paints = remember { arrayOfNulls<TermPaints>(1) }
    // Reading the version makes the drawing depend on it: the view redraws when the screen changed.
    val version = screen.version
    // For TalkBack: the last lines on screen. Computed here, from the version, so the semantics change with the screen.
    val description = remember(version) { "Terminal: " + screen.visibleText().lines().filter { it.isNotBlank() }.takeLast(20).joinToString(". ") }
    Canvas(
        modifier.fillMaxSize()
            .semantics { contentDescription = description }
            .pointerInput(Unit) {
                detectTransformGestures { _, pan, zoom, _ ->
                    if (zoom != 1f) userSp = (drawn[0] * zoom).coerceIn(MIN_SP, MAX_SP)
                    panX += pan.x
                    dragPx += pan.y
                    val cellH = cellAt(drawn[0] * drawn[1]).h
                    val whole = (dragPx / cellH).toInt()
                    if (whole != 0) {
                        screen.scrollBack = (screen.scrollBack + whole).coerceIn(0, screen.scrollbackRows)
                        dragPx -= whole * cellH
                    }
                }
            }
            .pointerInput(Unit) { detectTapGestures(onDoubleTap = { userSp = 0f; panX = 0f }) },
    ) {
        val emu = screen.emulator ?: return@Canvas
        version // dependency
        val pxPerSp = density * fontScale
        val sp = if (userSp > 0f) userSp else fitWidthSp(size.width, emu.mColumns, pxPerSp)
        drawn[0] = sp; drawn[1] = pxPerSp
        val px = sp * pxPerSp
        val p = paints[0]?.takeIf { it.px == px } ?: TermPaints(px).also { paints[0] = it }
        drawTerminal(emu, screen, theme, p, panX, emu.mColumns, emu.mRows)
    }
}

/** The text size (sp) at which [cols] columns fill [widthPx], not below a size that can be read (then it pans). */
fun fitWidthSp(widthPx: Float, cols: Int, pxPerSp: Float): Float {
    val unit = cellAt(100f).w / 100f
    return (widthPx / (cols.coerceAtLeast(1) * unit) / pxPerSp).coerceIn(READABLE_SP, MAX_SP)
}

private fun DrawScope.drawTerminal(emu: TerminalEmulator, screen: TermScreen, theme: TerminalColors, paints: TermPaints, panX: Float, cols: Int, rows: Int) {
    val cell = paints.cell
    val regular = paints.regular
    val bold = paints.bold
    val shift = bottomShift(size.height, rows, cell.h)
    val buf = emu.screen
    val top = -screen.scrollBack
    val canvas = drawContext.canvas.nativeCanvas
    val maxPan = max(0f, cols * cell.w - size.width)
    val x0 = panX.coerceIn(-maxPan, 0f)
    drawRect(theme.background)
    for (r in 0 until rows) {
        val y = r * cell.h + shift
        if (y + cell.h < 0f) continue
        val row = top + r
        if (row < -buf.activeTranscriptRows || row >= rows) continue
        val line = buf.allocateFullLineIfNecessary(buf.externalToInternalRow(row))
        val text = line.mText
        val used = line.spaceUsed
        var col = 0
        var ci = 0
        while (col < cols) {
            val style = line.getStyle(col)
            var fgv = TextStyle.decodeForeColor(style)
            var bgv = TextStyle.decodeBackColor(style)
            val fx = TextStyle.decodeEffect(style)
            val boldFx = fx and TextStyle.CHARACTER_ATTRIBUTE_BOLD != 0
            if (boldFx && fgv in 0..7) fgv += 8
            var fg = terminalColor(fgv, emu, theme)
            var bg = terminalColor(bgv, emu, theme)
            if (fx and TextStyle.CHARACTER_ATTRIBUTE_INVERSE != 0 != emu.isReverseVideo) { val t = fg; fg = bg; bg = t }
            if (fx and TextStyle.CHARACTER_ATTRIBUTE_DIM != 0) fg = fg.copy(alpha = 0.6f)
            var cp = ' '.code
            var chars = 0
            if (ci < used) {
                val ch = text[ci]
                if (Character.isHighSurrogate(ch) && ci + 1 < used) { cp = Character.toCodePoint(ch, text[ci + 1]); chars = 2 } else { cp = ch.code; chars = 1 }
            }
            val w = if (chars == 0) 1 else max(WcWidth.width(cp), 0)
            if (w == 0) { ci += chars; continue }
            val x = x0 + col * cell.w
            if (bg != theme.background) drawRect(bg, topLeft = androidx.compose.ui.geometry.Offset(x, y), size = androidx.compose.ui.geometry.Size(cell.w * w, cell.h))
            if (chars > 0 && cp != ' '.code && fx and TextStyle.CHARACTER_ATTRIBUTE_INVISIBLE == 0) {
                val p = if (boldFx) bold else regular
                p.color = fg.toArgb()
                val n = Character.toChars(cp, paints.chars, 0)
                canvas.drawText(paints.chars, 0, n, x, y + cell.baseline, p)
            }
            if (fx and TextStyle.CHARACTER_ATTRIBUTE_UNDERLINE != 0) drawRect(fg, topLeft = androidx.compose.ui.geometry.Offset(x, y + cell.h - 2f), size = androidx.compose.ui.geometry.Size(cell.w * w, 2f))
            if (fx and TextStyle.CHARACTER_ATTRIBUTE_STRIKETHROUGH != 0) drawRect(fg, topLeft = androidx.compose.ui.geometry.Offset(x, y + cell.h / 2), size = androidx.compose.ui.geometry.Size(cell.w * w, 2f))
            col += w
            ci += chars
        }
    }
    if (screen.scrollBack == 0 && emu.shouldCursorBeVisible()) {
        val cx = x0 + emu.cursorCol * cell.w
        val cy = emu.cursorRow * cell.h + shift
        drawRect(theme.cursor.copy(alpha = 0.55f), topLeft = androidx.compose.ui.geometry.Offset(cx, cy), size = androidx.compose.ui.geometry.Size(cell.w, cell.h))
    }
}
