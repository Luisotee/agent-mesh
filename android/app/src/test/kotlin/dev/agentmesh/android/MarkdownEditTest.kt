package dev.agentmesh.android

import dev.agentmesh.android.chat.Edit
import dev.agentmesh.android.chat.Field
import dev.agentmesh.android.chat.MarkdownEdit
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

class MarkdownEditTest {
    /** A field from text with `|` as the cursor, or `«` `»` around the selection (the fixture's marks). */
    private fun field(s: String): Field {
        val start = if ('«' in s) s.indexOf('«') else s.indexOf('|')
        val value = s.replace(Regex("[|«»]"), "")
        val end = if ('»' in s) s.indexOf('»') - 1 else start
        return Field(value, start, end)
    }

    private fun show(f: Field): String =
        if (f.start == f.end) f.value.substring(0, f.start) + "|" + f.value.substring(f.start)
        else f.value.substring(0, f.start) + "«" + f.value.substring(f.start, f.end) + "»" + f.value.substring(f.end)

    private fun press(input: String, edit: (Field) -> Edit?): String? {
        val f = field(input)
        return edit(f)?.let { show(MarkdownEdit.apply(f, it)) }
    }

    /** The cases the desktop's `composer-keys` is held to as well (`shared/markdown-edit/cases.json`). */
    @Test fun `every shared case gives the same field as on the desktop`() {
        val cases = Json.parseToJsonElement(File("../../shared/markdown-edit/cases.json").readText()).jsonObject["cases"]!!.jsonArray
        assertTrue(cases.size >= 20)
        for (c in cases) {
            val o = c.jsonObject
            val want = o["expected"].let { if (it == null || it is JsonNull) null else it.jsonPrimitive.content }
            val got = press(o["input"]!!.jsonPrimitive.content) { f ->
                when (val a = o["action"]!!.jsonPrimitive.content) {
                    "enter" -> MarkdownEdit.enter(f)
                    "tab" -> MarkdownEdit.indent(f, out = false)
                    "shift-tab" -> MarkdownEdit.indent(f, out = true)
                    "bold" -> MarkdownEdit.bold(f)
                    "italic" -> MarkdownEdit.italic(f)
                    "code" -> MarkdownEdit.code(f)
                    "link" -> MarkdownEdit.link(f)
                    else -> error("unknown action $a")
                }
            }
            assertEquals(o["name"]!!.jsonPrimitive.content, want, got)
        }
    }

    @Test fun `the list button makes each line an item, and a second press takes them back`() {
        val list = { s: String -> press(s) { MarkdownEdit.list(it, task = false) } }
        assertEquals("- a|", list("a|"))
        assertEquals("- «a\n- b»", list("«a\nb»"))
        assertEquals("«a\nb»", list("- «a\n- b»"))
        // A numbered line changes kind; a mix is all made one kind.
        assertEquals("- «one\n- two»", list("1. «one\n- two»"))
        assertEquals("  - a|", list("  a|"))
    }

    @Test fun `the checklist button makes task items and takes them back`() {
        val tasks = { s: String -> press(s) { MarkdownEdit.list(it, task = true) } }
        assertEquals("- [ ] a|", tasks("a|"))
        assertEquals("- [ ] «a\n- [ ] b»", tasks("«a\nb»"))
        assertEquals("«a\nb»", tasks("- [ ] «a\n- [x] b»"))
        // A plain bullet becomes a task.
        assertEquals("- [ ] a|", tasks("- a|"))
    }

    @Test fun `a selection ending on a newline does not take the next line in`() {
        assertEquals("- «a\n»b", press("«a\n»b") { MarkdownEdit.list(it, task = false) })
    }

    @Test fun `a list number of any length counts on instead of crashing`() {
        assertEquals("12345678901234567890. one\n12345678901234567891. |", press("12345678901234567890. one|") { MarkdownEdit.enter(it) })
    }

    @Test fun `blank lines inside a selection are not made items`() {
        assertEquals("- «a\n\n- b»", press("«a\n\nb»") { MarkdownEdit.list(it, task = false) })
        assertEquals("«a\n\nb»", press("- «a\n\n- b»") { MarkdownEdit.list(it, task = false) })
        // One blank line with the cursor in it is the user starting a list.
        assertEquals("- |", press("|") { MarkdownEdit.list(it, task = false) })
    }

    @Test fun `outdenting a selection never ends it before it starts`() {
        val f = field("  «a\n  b»")
        val e = MarkdownEdit.indent(f, out = true)!!
        assertTrue(e.selEnd >= e.selStart)
    }
}
