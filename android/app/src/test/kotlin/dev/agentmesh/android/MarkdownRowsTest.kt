package dev.agentmesh.android

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.hasTestTag
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performScrollToIndex
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.messages.MarkdownCache
import dev.agentmesh.android.ui.messages.MarkdownText
import dev.agentmesh.android.ui.messages.SYNC_PARSE_MAX
import dev.agentmesh.android.ui.messages.columnWidth
import kotlinx.coroutines.runBlocking
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/**
 * Why the chat jumped as it scrolled: a row's Markdown was parsed after the row was laid out (height 0, then its own), and parsed
 * again each time the row came back. Rows are now laid out at their height the first time and every time; and the tables a phone can read.
 */
@RunWith(RobolectricTestRunner::class)
@org.robolectric.annotation.GraphicsMode(org.robolectric.annotation.GraphicsMode.Mode.NATIVE)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi")
class MarkdownRowsTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))

    @Before @After fun fresh() { MarkdownCache.clear() }

    private fun body(i: Int) = "Reply $i, with **bold** text.\n\nA second paragraph.\n\n- one\n- two\n- three"
    private val long = (1..40).joinToString("\n\n") { "Paragraph $it of a long answer, with `code` and **bold** text in it." }

    @Test fun `the cache hands back the same parse, parses only what asks for it, and lets the oldest go`() {
        val a = MarkdownCache.parse("# a")
        assertNotNull(a)
        assertSame(a, MarkdownCache.parse("# a"))
        runBlocking { MarkdownCache.warm(listOf("plain words", "**b**")) }
        assertNull(MarkdownCache.get("plain words"))
        assertNotNull(MarkdownCache.get("**b**"))
        repeat(MarkdownCache.MAX_ENTRIES + 20) { MarkdownCache.parse("# row $it") }
        assertEquals(MarkdownCache.MAX_ENTRIES, MarkdownCache.size())
        assertNull(MarkdownCache.get("# a"))
        assertNotNull(MarkdownCache.get("# row ${MarkdownCache.MAX_ENTRIES + 19}"))
    }

    @Test fun `a row is as tall when it first appears as it ever is, also after scrolling away and back`() {
        val seen = mutableMapOf<Int, MutableList<Int>>()
        compose.setContent {
            AgentMeshTheme(theme) {
                val list = rememberLazyListState()
                LazyColumn(Modifier.fillMaxSize().testTag("list"), state = list) {
                    items(30, key = { it }) { i -> Column(Modifier.fillMaxWidth().onSizeChanged { seen.getOrPut(i) { mutableListOf() }.add(it.height) }) { MarkdownText(body(i)) } }
                }
            }
        }
        compose.waitForIdle()
        compose.onNodeWithTag("list").performScrollToIndex(25)
        compose.waitForIdle()
        compose.onNodeWithTag("list").performScrollToIndex(0)
        compose.waitForIdle()
        compose.onNodeWithTag("list").performScrollToIndex(25)
        compose.waitForIdle()
        for ((i, heights) in seen) {
            val distinct = heights.distinct()
            assertEquals("row $i changed height: $heights", 1, distinct.size)
            assertTrue("row $i was laid out empty: $heights", distinct.single() > 0)
        }
        assertTrue(seen.size > 6)
    }

    @Test fun `a long body shows a placeholder once, and is laid out at its height at once when it comes back`() {
        val heights = mutableListOf<Int>()
        var show by mutableStateOf(true)
        compose.setContent {
            AgentMeshTheme(theme) { if (show) Column(Modifier.fillMaxWidth().onSizeChanged { heights.add(it.height) }) { MarkdownText(long) } }
        }
        assertTrue(long.length > SYNC_PARSE_MAX)
        compose.waitUntil(10_000) { MarkdownCache.get(long) != null }
        compose.waitForIdle()
        show = false; compose.waitForIdle()
        heights.clear()
        show = true; compose.waitForIdle()
        assertEquals("the second time it is its own height at once: $heights", 1, heights.distinct().size)
        assertTrue(heights.first() > 24 * 40)
    }

    @Test fun `a table's long cell wraps, and the table scrolls sideways rather than cutting its columns`() {
        val md = "| Option | What it does | Cost | Notes |\n|---|---|---|---|\n| Jitter | Adds a random delay to every retry so that clients do not all retry together after an outage | low | Recommended |\n| Fixed | Waits | none | Flaky |"
        compose.setContent { AgentMeshTheme(theme) { Column(Modifier.fillMaxWidth()) { MarkdownText(md) } } }
        compose.waitForIdle()
        compose.onAllNodesWithTag("md-table").assertCountEquals(1)
        val long = compose.onNodeWithText("Adds a random delay", substring = true, useUnmergedTree = true).fetchSemanticsNode().boundsInRoot
        val short = compose.onNodeWithText("Waits", substring = true, useUnmergedTree = true).fetchSemanticsNode().boundsInRoot
        assertTrue("the long cell is ${long.height} px high, a one-line cell ${short.height}: it did not wrap", long.height > short.height * 2)
        // Four columns of their own widths are wider than the phone: the table holds them and scrolls.
        val table = compose.onNodeWithTag("md-table").fetchSemanticsNode()
        assertTrue(table.boundsInRoot.width > 0)
        compose.onNodeWithText("Notes", substring = true, useUnmergedTree = true).assertExists()
        compose.onNodeWithText("Recommended", substring = true, useUnmergedTree = true).assertExists()
    }

    @Test fun `columns are as wide as their text, within a floor and a cap`() {
        assertEquals(64f, columnWidth(1).value, 0.01f)
        assertEquals(240f, columnWidth(500).value, 0.01f)
        assertTrue(columnWidth(20).value in 150f..200f)
    }

    private val tmp = org.junit.rules.TemporaryFolder().also { it.create() }
    private val api = RecordingApi()
    private val started = mutableListOf<dev.agentmesh.android.HostRuntime>()

    private fun chatWith(result: String, truncated: Boolean) {
        fun e(id: String, kind: dev.agentmesh.gen.agentmesh.v1.TranscriptKind, text: String = "", tool: String = "", input: String = "") =
            dev.agentmesh.gen.agentmesh.v1.TranscriptEntry.newBuilder().setId(id).setKind(kind).setText(text).setTool(tool).setToolInput(input).setToolUseId("t1").setTruncated(truncated && kind == dev.agentmesh.gen.agentmesh.v1.TranscriptKind.TRANSCRIPT_KIND_TOOL_RESULT).build()
        val K = dev.agentmesh.gen.agentmesh.v1.TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL
        val R = dev.agentmesh.gen.agentmesh.v1.TranscriptKind.TRANSCRIPT_KIND_TOOL_RESULT
        api.transcript = { _, _, _ -> dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse.newBuilder().setSupported(true).setSessionId("s1").addEntries(e("1", K, tool = "Bash", input = "ls")).addEntries(e("2", R, text = result)).setStartOffset(0).setEndOffset(10).setAtStart(true).build() }
        val repo = dev.agentmesh.android.hosts.HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(dev.agentmesh.android.net.Route.Direct("http://127.0.0.1:1")), "t")
        val rt = dev.agentmesh.android.HostRuntime(host.id, repo, { _, _ -> api }, kotlinx.coroutines.CoroutineScope(kotlinx.coroutines.Dispatchers.Unconfined))
        rt.start(); started += rt
        rt.onState(dev.agentmesh.android.store.ConnState.Ready(dev.agentmesh.gen.agentmesh.v1.HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1")), listOf(agent("a1", name = "shop-fix", state = dev.agentmesh.gen.agentmesh.v1.AgentState.AGENT_STATE_IDLE).toBuilder().setKind("claude").setTerminalId("t-a1").build())))
        val container = AppContainer(androidx.test.core.app.ApplicationProvider.getApplicationContext<android.app.Application>(), Plain, newApi = { _, _ -> api })
        compose.setContent { androidx.compose.runtime.CompositionLocalProvider(dev.agentmesh.android.ui.LocalContainer provides container) { AgentMeshTheme(theme) { dev.agentmesh.android.ui.AgentScreen(rt, "a1", {}, dev.agentmesh.android.notify.Tab.Chat) } } }
        compose.waitUntil(5_000) { compose.onAllNodes(hasTestTag("chat-tools")).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("chat-tools").performClick()
        compose.waitForIdle()
    }

    @Test fun `a tool's answer that is cut to eight lines by wrapping offers Show all, a short one does not`() {
        // One line of 350 characters: no newline, under the old 400-character rule, and about a dozen lines on a phone.
        chatWith("x".repeat(30).let { w -> (1..12).joinToString(" ") { w } }, truncated = false)
        compose.onNodeWithTag("tool-result-toggle").assertExists().performClick()
        compose.onNodeWithText("Show less").assertExists()
        started.forEach { it.stop() }
    }

    @Test fun `a short answer has no Show all`() {
        chatWith("done", truncated = false)
        compose.onAllNodesWithTag("tool-result-toggle").assertCountEquals(0)
        started.forEach { it.stop() }
    }

    @Test fun `an answer the host cut says so without being expanded`() {
        chatWith("three\nshort\nlines", truncated = true)
        compose.onNodeWithText("cut here", substring = true, useUnmergedTree = true).assertExists()
        compose.onAllNodesWithTag("tool-result-toggle").assertCountEquals(0)
        started.forEach { it.stop() }
    }
}
