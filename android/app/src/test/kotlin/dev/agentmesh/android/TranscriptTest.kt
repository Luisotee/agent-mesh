package dev.agentmesh.android

import dev.agentmesh.android.chat.Block
import dev.agentmesh.android.chat.Followed
import dev.agentmesh.android.chat.Transcript
import dev.agentmesh.android.chat.fromTail
import dev.agentmesh.android.chat.toBlocks
import dev.agentmesh.android.chat.toolsSummary
import dev.agentmesh.android.chat.withEarlier
import dev.agentmesh.android.chat.withLater
import dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse
import dev.agentmesh.gen.agentmesh.v1.TranscriptEntry
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind
import org.junit.Assert.assertEquals
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

/** The chat's transcript logic: the desktop's `transcript.test.ts`, case for case. */
class TranscriptTest {
    private fun entry(id: String, kind: TranscriptKind, text: String = "", tool: String = "", toolInput: String = "", toolUseId: String = "", isError: Boolean = false): TranscriptEntry =
        TranscriptEntry.newBuilder().setId(id).setKind(kind).setText(text).setTool(tool).setToolInput(toolInput).setToolUseId(toolUseId).setIsError(isError).build()

    private fun page(entries: List<TranscriptEntry>, sessionId: String = "s1", start: Long = 0, end: Long = 100, atStart: Boolean = false, supported: Boolean = true): ReadTranscriptResponse =
        ReadTranscriptResponse.newBuilder().setSupported(supported).setSessionId(sessionId).addAllEntries(entries).setStartOffset(start).setEndOffset(end).setAtStart(atStart).build()

    private val held = fromTail(page(listOf(entry("a", TranscriptKind.TRANSCRIPT_KIND_USER, "hi")), start = 50, end = 100))

    @Test fun `puts earlier entries before and later ones after, and moves its ends`() {
        val back = withEarlier(held, page(listOf(entry("z", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT)), start = 0, end = 50, atStart = true))
        assertEquals(listOf("z", "a"), back.entries.map { it.id })
        assertEquals(0L, back.start); assertEquals(100L, back.end); assertTrue(back.atStart)
        val next = withLater(held, page(listOf(entry("b", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT)), start = 100, end = 160)) as Followed.More
        assertEquals(listOf("a", "b"), next.transcript.entries.map { it.id })
        assertEquals(160L, next.transcript.end)
    }

    @Test fun `has nothing to do when nothing is new`() {
        assertEquals(Followed.Same, withLater(held, page(emptyList(), start = 100, end = 100)))
    }

    @Test fun `starts over for another conversation or a record that got shorter, and ignores earlier entries of another one`() {
        assertEquals(Followed.Reload, withLater(held, page(emptyList(), sessionId = "s2", end = 10)))
        // A read that began elsewhere than where the held entries end is not appended.
        assertEquals(Followed.Same, withLater(held, page(listOf(entry("b", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT)), start = 40, end = 160), from = 40))
        assertEquals(Followed.Reload, withLater(held, page(emptyList(), end = 60)))
        assertSame(held, withEarlier(held, page(listOf(entry("z", TranscriptKind.TRANSCRIPT_KIND_USER)), sessionId = "s2")))
        // A page that doesn't end where the held entries begin (in flight across a reload) would repeat entries.
        assertSame(held, withEarlier(held, page(listOf(entry("z", TranscriptKind.TRANSCRIPT_KIND_USER)), start = 0, end = 20)))
    }

    @Test fun `knows a CLI the host reads no record for`() {
        val t = fromTail(page(emptyList(), supported = false))
        assertEquals(false, t.supported); assertTrue(t.entries.isEmpty())
        assertTrue(Transcript().supported)
    }

    @Test fun `folds consecutive tool calls into one block, each with its own result`() {
        val blocks = toBlocks(
            listOf(
                entry("1", TranscriptKind.TRANSCRIPT_KIND_USER, "go"),
                entry("2", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "ok"),
                entry("3", TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL, tool = "Bash", toolInput = "ls", toolUseId = "t1"),
                entry("4", TranscriptKind.TRANSCRIPT_KIND_TOOL_RESULT, "a b", toolUseId = "t1"),
                entry("5", TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL, tool = "Read", toolInput = "/x", toolUseId = "t2"),
                entry("6", TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL, tool = "Bash", toolInput = "pwd", toolUseId = "t3"),
                entry("7", TranscriptKind.TRANSCRIPT_KIND_TOOL_RESULT, "boom", toolUseId = "t3", isError = true),
                entry("8", TranscriptKind.TRANSCRIPT_KIND_TOOL_RESULT, "file", toolUseId = "t2"),
                entry("9", TranscriptKind.TRANSCRIPT_KIND_MESH, "📬 [mesh] …"),
                entry("10", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "done"),
            ),
        )
        assertEquals(listOf("User", "Assistant", "Tools", "Mesh", "Assistant"), blocks.map { it::class.simpleName })
        val tools = blocks[2] as Block.Tools
        assertEquals(listOf(Triple("Bash", "ls", "a b"), Triple("Read", "/x", "file"), Triple("Bash", "pwd", "boom")), tools.calls.map { Triple(it.tool, it.input, it.result?.text) })
        assertTrue(tools.calls[2].result!!.isError)
        assertEquals("Ran 3 tools: Bash ×2, Read", toolsSummary(tools.calls))
    }

    @Test fun `shows a result whose call is before the window, and a call still waiting for its result`() {
        val b = toBlocks(listOf(entry("r", TranscriptKind.TRANSCRIPT_KIND_TOOL_RESULT, "late", toolUseId = "gone"), entry("c", TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL, tool = "Bash", toolUseId = "open")))[0] as Block.Tools
        assertEquals(listOf("tool" to "late", "Bash" to null), b.calls.map { it.tool to it.result?.text })
        assertEquals("Ran 1 tool: Bash", toolsSummary(b.calls.drop(1)))
    }
}
