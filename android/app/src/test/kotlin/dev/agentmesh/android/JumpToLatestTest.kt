package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.hasTestTag
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performTouchInput
import androidx.compose.ui.test.swipeDown
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AgentScreen
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse
import dev.agentmesh.gen.agentmesh.v1.TranscriptEntry
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** A reader up in the history is told when the chat moves on below, and one tap takes them to the end. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi")
class JumpToLatestTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val api = RecordingApi()
    private val started = mutableListOf<HostRuntime>()
    @After fun stop() { started.forEach { it.stop() } }

    private fun e(i: Int) = TranscriptEntry.newBuilder().setId("e$i").setKind(TranscriptKind.TRANSCRIPT_KIND_ASSISTANT).setText("Reply number $i, in plain words.").build()
    private fun page(range: IntRange, start: Long, end: Long) = ReadTranscriptResponse.newBuilder().setSupported(true).setSessionId("s1").addAllEntries(range.map(::e)).setStartOffset(start).setEndOffset(end).setAtStart(true).build()

    @Test fun `new entries below a reader who scrolled up show a pill, and a tap goes to the end`() {
        var more = false
        api.transcript = { _, off, back -> if (back || !more) page(1..40, 0, 100) else page(41..43, off, off + 50) }
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined))
        rt.start(); started += rt
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1")), listOf(agent("a1", name = "shop-fix", state = AgentState.AGENT_STATE_WORKING).toBuilder().setKind("claude").setTerminalId("t-a1").build())))
        val container = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> api })
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AgentScreen(rt, "a1", {}, Tab.Chat) } } }
        compose.waitUntil(5_000) { compose.onAllNodes(hasTestTag("chat-assistant")).fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodesWithTag("jump-to-latest").assertCountEquals(0)
        // The reader goes up into the history: nothing new yet, no pill.
        // By touch, as a reader does (a semantic scroll is not a drag, which is what says the reader left the end).
        repeat(4) { compose.onNodeWithTag("chat").performTouchInput { swipeDown() }; compose.waitForIdle() }
        compose.onAllNodesWithTag("jump-to-latest").assertCountEquals(0)
        // The agent says more (the poll reads on while it works).
        more = true
        compose.waitUntil(15_000) { compose.onAllNodesWithTag("jump-to-latest").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("jump-to-latest").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("jump-to-latest").fetchSemanticsNodes().isEmpty() }
        // At the end again, following it.
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Reply number 43", substring = true)).fetchSemanticsNodes().isNotEmpty() }
    }

    @Test fun `a reader at the end has no pill`() {
        api.transcript = { _, _, _ -> page(1..3, 0, 100) }
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined))
        rt.start(); started += rt
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1")), listOf(agent("a1", name = "shop-fix", state = AgentState.AGENT_STATE_IDLE).toBuilder().setKind("claude").setTerminalId("t-a1").build())))
        val container = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> api })
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AgentScreen(rt, "a1", {}, Tab.Chat) } } }
        compose.waitUntil(5_000) { compose.onAllNodes(hasTestTag("chat-assistant")).fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodesWithTag("jump-to-latest").assertCountEquals(0)
    }

    private fun entries(vararg ids: String) = ids.map { id -> dev.agentmesh.android.chat.Entry.Chat(dev.agentmesh.android.chat.Block.Assistant(e(id.removePrefix("e").toInt()))) }

    @Test fun `what is new is counted after the entry the reader was at the end for, and from the new tail when a reload took it away`() {
        val list = entries("e1", "e2", "e3", "e4")
        assertEquals(2, dev.agentmesh.android.chat.unseenCount(list, "e2", stick = false))
        assertEquals(0, dev.agentmesh.android.chat.unseenCount(list, "e2", stick = true))
        assertEquals(0, dev.agentmesh.android.chat.unseenCount(list, null, stick = false))
        // The conversation was reloaded under a reader who is up in the history: the old place is gone.
        val reloaded = entries("e11", "e12")
        assertEquals(0, dev.agentmesh.android.chat.unseenCount(reloaded, "e2", stick = false))
        assertTrue(dev.agentmesh.android.chat.reanchor(reloaded, "e2"))
        assertTrue(!dev.agentmesh.android.chat.reanchor(list, "e2")); assertTrue(!dev.agentmesh.android.chat.reanchor(list, null))
        // After the reload the pill counts again, from the new tail.
        assertEquals(1, dev.agentmesh.android.chat.unseenCount(entries("e11", "e12", "e13"), "e12", stick = false))
    }
}
