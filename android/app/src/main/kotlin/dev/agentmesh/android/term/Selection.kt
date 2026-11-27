package dev.agentmesh.android.term

/** A cell of the terminal: a column, and a row counted from the first line of the whole history (so it holds while output scrolls). */
data class CellPos(val col: Int, val row: Long)

/**
 * A selection of text on the terminal: from [anchor] to [focus], either way round, as a stream of cells the way a terminal
 * selects (the first row from its start column to the end of the line, whole rows between, the last up to its end column).
 */
data class Selection(val anchor: CellPos, val focus: CellPos) {
    val start: CellPos get() = if (before(anchor, focus)) anchor else focus
    val end: CellPos get() = if (before(anchor, focus)) focus else anchor

    /** Whether a cell is inside it. */
    fun contains(col: Int, row: Long, cols: Int): Boolean {
        val s = start; val e = end
        if (row < s.row || row > e.row) return false
        val from = if (row == s.row) s.col else 0
        val to = if (row == e.row) e.col else cols - 1
        return col in from..to
    }

    private fun before(a: CellPos, b: CellPos) = a.row < b.row || (a.row == b.row && a.col <= b.col)

    companion object {
        /** The word around [col] in [text] (a run of non-space characters), as its first and last column; a blank cell selects itself. */
        fun wordAt(text: String, col: Int): IntRange {
            if (col !in text.indices || text[col].isWhitespace()) return col..col
            var a = col
            var b = col
            while (a > 0 && !text[a - 1].isWhitespace()) a--
            while (b < text.length - 1 && !text[b + 1].isWhitespace()) b++
            return a..b
        }

        /** The cell under a point: [x],[y] in the canvas, for cells of [cellW] by [cellH] shifted by [panX] and [shiftY], the top row being [topRow]. */
        fun cellAt(x: Float, y: Float, cellW: Float, cellH: Float, panX: Float, shiftY: Float, topRow: Long, cols: Int, rows: Int): CellPos {
            val c = ((x - panX) / cellW).toInt().coerceIn(0, cols - 1)
            val r = ((y - shiftY) / cellH).toInt().coerceIn(0, rows - 1)
            return CellPos(c, topRow + r)
        }
    }
}
