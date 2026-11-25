package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.hasContentDescription
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onRoot
import androidx.compose.ui.test.printToLog
import androidx.compose.ui.test.onNodeWithText
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.term.TermState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AgentScreen
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.fitWidthSp
import dev.agentmesh.android.ui.terminalColor
import dev.agentmesh.android.ui.termSentence
import dev.agentmesh.gen.agentmesh.term.v1.AttachMode
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi")
class TerminalTabTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()

    private val spec = Themes.parse(File("src/main/assets/themes/dracula.json").readText())
    private val theme = Themes.resolve(spec)
    private val api = RecordingApi()

    private fun runtime(): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        return HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined)).also {
            it.connection.start() // builds the client the tab asks for (its handshake never answers)
            it.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
            it.onFrame(snapshot(listOf(run("r1")), listOf(agent("a1", name = "shop-fix", state = AgentState.AGENT_STATE_WORKING))))
        }
    }

    private fun show(rt: HostRuntime) {
        val container = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> api })
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AgentScreen(rt, "a1", {}, Tab.Terminal) } } }
    }

    @Test fun `the terminal tab shows the host's screen live and says it is read only`() {
        val s = FakeSocket().also { api.sockets += it }
        val rt = runtime()
        show(rt)
        compose.waitUntil(5_000) { api.tickets.isNotEmpty() }
        s.host(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, offset = 21, cols = 40, rows = 10))
        s.host(Frames.screen("fake claude s1\r\n> ", offset = 21)); s.host(Frames.replayDone(21))
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live")).fetchSemanticsNodes().isNotEmpty() }
        compose.waitForIdle()
        compose.onNodeWithText("Read only").assertIsDisplayed()
        val descs = compose.onAllNodes(hasContentDescription("Terminal", substring = true)).fetchSemanticsNodes().map { it.config[androidx.compose.ui.semantics.SemanticsProperties.ContentDescription] }
        assertEquals(listOf(listOf("Terminal: fake claude s1. >")), descs)
        val node = compose.onNode(hasContentDescription("Terminal: fake claude s1. >", substring = true)).fetchSemanticsNode()
        assertTrue("terminal canvas size ${node.size}", node.size.width > 0 && node.size.height > 0)
        // The screen came to this tab only: nothing of it is sent but the attach.
        assertEquals(1, s.sent.size)
    }

    @Test fun `an agent with no terminal says why instead of showing a blank`() {
        api.terminalError = dev.agentmesh.android.net.ApiException(com.connectrpc.Code.NOT_FOUND, "no terminal for agent a1")
        val rt = runtime()
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("no terminal for agent a1")).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("terminal-state").assertIsDisplayed()
    }

    @Test fun `state sentences are plain`() {
        assertEquals("Live", termSentence(TermState.Live))
        assertEquals("Reconnecting (2): reset", termSentence(TermState.Reconnecting(2, "reset")))
        assertEquals("The agent's process exited with code 3", termSentence(TermState.Exited(3, 0)))
        assertEquals("The agent's process ended", termSentence(TermState.Exited(0, 0)))
    }

    @Test fun `a program's colours map into the theme, and the width fits the screen`() {
        val screen = dev.agentmesh.android.term.TermScreen().also { it.reset(20, 5) }
        val emu = screen.emulator!!
        val t = theme.terminal
        assertEquals(t.ansi[1], terminalColor(1, emu, t))
        assertEquals(t.foreground, terminalColor(com.termux.terminal.TextStyle.COLOR_INDEX_FOREGROUND, emu, t))
        assertEquals(t.background, terminalColor(com.termux.terminal.TextStyle.COLOR_INDEX_BACKGROUND, emu, t))
        assertEquals(androidx.compose.ui.graphics.Color(0xFF0A141E), terminalColor(0xff0a141e.toInt(), emu, t))
        // A palette colour above 15 comes from the emulator's 256-colour table.
        assertTrue(terminalColor(196, emu, t).red > 0.9f)
        // Fitting the host's width: a wider terminal gets a smaller size, never below what can be read or above a
        // comfortable one. (The size itself depends on the font's metrics, which the JVM doesn't have.)
        val w = 411f * 2.75f
        val wide = fitWidthSp(w, 500, 2.75f)
        val narrow = fitWidthSp(w, 40, 2.75f)
        assertTrue("$wide <= $narrow", wide <= narrow)
        assertTrue(wide >= 8f && narrow <= 28f)
    }
}
