package dev.agentmesh.android.chat

/**
 * The Markdown edits of a multi-line field (the chat's composer, a message, an answer): pure, over the text and the
 * selection, so each is checked without a screen. The rules for Enter, Tab and the wrapping keys are the desktop's
 * (`desktop/src/composer-keys.ts`) and are pinned by one shared file, `shared/markdown-edit/cases.json`, that this app's
 * test reads (the desktop's can read it too, `TODO.md`); the list and checklist buttons are the phone's own.
 */
data class Field(val value: String, val start: Int, val end: Int)

/** Replace [from]..[to] with [text], then select [selStart]..[selEnd] (offsets in the new value). */
data class Edit(val from: Int, val to: Int, val text: String, val selStart: Int, val selEnd: Int)

object MarkdownEdit {
    /** A list item's start: its indent, its marker (`-`, `*`, `+`, `1.`, `1)`), the gap and a task box (`[ ]`, `[x]`). */
    private val ITEM = Regex("^(\\s*)([-*+]|\\d+[.)])(\\s+)(\\[[ xX]\\]\\s+)?")

    private fun lineStart(v: String, at: Int) = v.lastIndexOf('\n', at - 1) + 1
    private fun lineEnd(v: String, at: Int): Int = v.indexOf('\n', at).let { if (it == -1) v.length else it }

    /** The marker the next item takes: the same bullet, the next number, an empty task box. */
    private fun nextMarker(m: MatchResult): String {
        val (indent, marker, gap, task) = m.destructured
        val num = Regex("^(\\d+)([.)])$").find(marker)
        // A number of any length (pasted digits) counts on: no fixed-size integer to overflow.
        val next = if (num != null) "${num.groupValues[1].toBigInteger() + java.math.BigInteger.ONE}${num.groupValues[2]}" else marker
        return "$indent$next$gap${if (task.isNotEmpty()) "[ ] " else ""}"
    }

    /**
     * Enter on a list item: the next item, or on an empty one the end of the list. Null for anything else, which the field
     * handles itself (it adds a line here; the desktop sends).
     */
    fun enter(f: Field): Edit? {
        val ls = lineStart(f.value, f.start)
        val line = f.value.substring(ls, lineEnd(f.value, f.start))
        val m = ITEM.find(line) ?: return null
        if (f.start != f.end) return null
        // An empty item ends the list: its marker goes, and the line is left empty.
        if (line.trim() == m.value.trim()) return Edit(ls, ls + line.length, "", ls, ls)
        if (f.start >= ls + m.value.length) {
            val text = "\n${nextMarker(m)}"
            return Edit(f.start, f.end, text, f.start + text.length, f.start + text.length)
        }
        return null
    }

    /**
     * Tab and Shift+Tab: indent or outdent every line the selection touches by two spaces. Only on a list item or a
     * selection: elsewhere null, so the key is left alone.
     */
    fun indent(f: Field, out: Boolean): Edit? {
        val from = lineStart(f.value, f.start)
        if (f.start == f.end && !ITEM.containsMatchIn(f.value.substring(from, lineEnd(f.value, f.start)))) return null
        val to = lineEnd(f.value, if (f.end > f.start && f.value[f.end - 1] == '\n') f.end - 1 else f.end)
        val lines = f.value.substring(from, to).split('\n')
        var first = 0
        var total = 0
        val next = lines.mapIndexed { i, l ->
            val d: Int
            val r: String
            if (out) {
                d = -minOf(2, l.length - l.trimStart().length)
                r = l.substring(-d)
            } else {
                d = 2
                r = "  $l"
            }
            if (i == 0) first = d
            total += d
            r
        }
        if (out && total == 0) return null
        val selStart = maxOf(from, f.start + first)
        val selEnd = if (f.start == f.end) selStart else maxOf(selStart, f.end + total)
        return Edit(from, to, next.joinToString("\n"), selStart, selEnd)
    }

    /** Wraps the selection in [left]/[right] (or unwraps it when it is wrapped so already); with none, puts the cursor between. */
    fun wrap(f: Field, left: String, right: String = left): Edit {
        val sel = f.value.substring(f.start, f.end)
        val before = f.value.substring(maxOf(0, f.start - left.length), f.start)
        val after = f.value.substring(f.end, minOf(f.value.length, f.end + right.length))
        if (sel.isNotEmpty() && before == left && after == right) return Edit(f.start - left.length, f.end + right.length, sel, f.start - left.length, f.end - left.length)
        // The selection takes the markers in too (all of `**x**` selected): it is unwrapped the same. Only one span:
        // `**a** and **b**` (or `__init__`) is wrapped, not broken.
        if (sel.length >= left.length + right.length && sel.startsWith(left) && sel.endsWith(right)) {
            val inner = sel.substring(left.length, sel.length - right.length)
            if (!inner.contains(left) && !inner.contains(right)) return Edit(f.start, f.end, inner, f.start, f.start + inner.length)
        }
        return Edit(f.start, f.end, "$left$sel$right", f.start + left.length, f.start + left.length + sel.length)
    }

    fun bold(f: Field) = wrap(f, "**")
    fun italic(f: Field) = wrap(f, "_")
    fun code(f: Field) = wrap(f, "`")

    /** A link: the selection as its text, the cursor where the address goes. */
    fun link(f: Field): Edit {
        val sel = f.value.substring(f.start, f.end)
        val text = "[$sel]()"
        val at = f.start + text.length - 1
        return Edit(f.start, f.end, text, if (sel.isNotEmpty()) at else f.start + 1, if (sel.isNotEmpty()) at else f.start + 1)
    }

    /**
     * The list and checklist buttons: every line the selection touches becomes an item (`- ` or `- [ ] `), or, when all of
     * them are already of that kind, a plain line again. A line of another kind of item (a number, a bullet) changes kind.
     */
    fun list(f: Field, task: Boolean): Edit {
        val from = lineStart(f.value, f.start)
        val to = lineEnd(f.value, if (f.end > f.start && f.value[f.end - 1] == '\n') f.end - 1 else f.end)
        val lines = f.value.substring(from, to).split('\n')
        fun kind(l: String): Boolean = ITEM.find(l)?.let { m -> m.groupValues[2] == "-" && (m.groupValues[4].isNotEmpty() == task) } ?: false
        // A blank line inside a selection of several is left as it is (it would become an empty item).
        val skip = lines.size > 1
        val remove = lines.filter { !skip || it.isNotBlank() }.all(::kind)
        var first = 0
        var total = 0
        val next = lines.mapIndexed { i, l ->
            if (skip && l.isBlank()) { if (i == 0) first = 0; return@mapIndexed l }
            val m = ITEM.find(l)
            val indent = m?.groupValues?.get(1) ?: l.takeWhile { it == ' ' || it == '\t' }
            val body = if (m != null) l.substring(m.value.length) else l.substring(indent.length)
            val r = if (remove) "$indent$body" else "$indent${if (task) "- [ ] " else "- "}$body"
            val d = r.length - l.length
            if (i == 0) first = d
            total += d
            r
        }
        val selStart = maxOf(from, f.start + first)
        val selEnd = if (f.start == f.end) selStart else maxOf(selStart, f.end + total)
        return Edit(from, to, next.joinToString("\n"), selStart, selEnd)
    }

    /** The field after an edit. */
    fun apply(f: Field, e: Edit): Field = Field(f.value.substring(0, e.from) + e.text + f.value.substring(e.to), e.selStart, e.selEnd)
}
