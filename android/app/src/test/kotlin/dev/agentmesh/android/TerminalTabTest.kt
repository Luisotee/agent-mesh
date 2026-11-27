package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.click
import androidx.compose.ui.test.longClick
import androidx.compose.ui.test.performTouchInput
import androidx.compose.ui.test.performTextInput
import androidx.compose.ui.test.performKeyInput
import androidx.compose.ui.test.pressKey
import androidx.compose.ui.test.performScrollTo
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
import dev.agentmesh.android.ui.clampPan
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

    private val spec = Themes.parse(File("../../shared/themes/dracula.json").readText())
    private val theme = Themes.resolve(spec)
    private val api = RecordingApi()

    private fun runtime(actor: dev.agentmesh.gen.agentmesh.v1.ActorClass = dev.agentmesh.gen.agentmesh.v1.ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        return HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined)).also {
            it.connection.start() // builds the client the tab asks for (its handshake never answers)
            it.onState(ConnState.Ready(HelloResponse.newBuilder().setActor(dev.agentmesh.gen.agentmesh.v1.Actor.newBuilder().setClass_(actor).setId("device-1")).build()))
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
        // The description follows the screen a few times a second, not on every frame.
        compose.waitUntil(5_000) { compose.onAllNodes(hasContentDescription("fake claude s1", substring = true)).fetchSemanticsNodes().isNotEmpty() }
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

    @Test fun `a failed attach offers Try again, which attaches again`() {
        api.terminalError = dev.agentmesh.android.net.ApiException(com.connectrpc.Code.FAILED_PRECONDITION, "agent a1 has no terminal")
        val rt = runtime()
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("agent a1 has no terminal")).fetchSemanticsNodes().isNotEmpty() }
        // The terminal shows up a moment later.
        api.terminalError = null
        val s = FakeSocket().also { api.sockets += it }
        compose.onNodeWithTag("terminal-retry").performClick()
        compose.waitUntil(5_000) { api.tickets.isNotEmpty() }
        s.host(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, offset = 3, cols = 40, rows = 10)); s.host(Frames.screen("up", offset = 3)); s.host(Frames.replayDone(3))
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live")).fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodes(hasText("Try again")).assertCountEquals(0)
    }

    @Test fun `until the host answers, the status says it is waiting for the host`() {
        val repo = HostRepository(tmp.newFile("hosts2.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        // No Ready state: the connection never answers.
        val rt = HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined)).also {
            it.connection.start()
            it.onFrame(snapshot(listOf(run("r1")), listOf(agent("a1", name = "shop-fix", state = AgentState.AGENT_STATE_WORKING))))
        }
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Waiting for the host…")).fetchSemanticsNodes().isNotEmpty() }
        assertEquals("Waiting for the host…", termSentence(TermState.WaitingForHost))
    }

    @Test fun `a sideways pan stays within the screen, so a drag past the edge doesn't need dragging back`() {
        // 80 columns of 10 px on a 600 px canvas: 200 px wider.
        assertEquals(0f, clampPan(300f, 80, 10f, 600f))
        assertEquals(-200f, clampPan(-900f, 80, 10f, 600f))
        assertEquals(-50f, clampPan(-50f, 80, 10f, 600f))
        // It all fits: no pan at all, however far it was dragged.
        assertEquals(0f, clampPan(-400f, 40, 10f, 600f))
    }

    @Test fun `a terminal taller than the canvas keeps its bottom in view`() {
        assertEquals(0f, dev.agentmesh.android.ui.bottomShift(900f, 30, 20f))
        assertEquals(-300f, dev.agentmesh.android.ui.bottomShift(900f, 60, 20f))
    }

    @Test fun `state sentences are plain`() {
        assertEquals("The agent's process ended", termSentence(TermState.Ended))
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

    // ── Typing ──

    private fun live(s: FakeSocket, input: Boolean) {
        s.host(dev.agentmesh.gen.agentmesh.term.v1.TermFrame.newBuilder().setAttached(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, offset = 21, cols = 40, rows = 10).attached.toBuilder().setInput(input)).build())
        s.host(Frames.screen("fake claude s1\r\n> ", offset = 21)); s.host(Frames.replayDone(21))
    }

    @Test fun `a read-only device has no keyboard, no key row, and never asks for the input right`() {
        val s = FakeSocket().also { api.sockets += it }
        val rt = runtime(dev.agentmesh.gen.agentmesh.v1.ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE)
        show(rt)
        compose.waitUntil(5_000) { api.tickets.isNotEmpty() }
        live(s, input = false)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live")).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("read-only").assertIsDisplayed()
        compose.onAllNodesWithTag("keyboard-toggle").assertCountEquals(0)
        compose.onAllNodesWithTag("key-row").assertCountEquals(0)
        assertEquals(listOf(false), api.ticketInputs)
    }

    @Test fun `a device that may type has a Keyboard switch, and with it on asks for the input right and shows the key row`() {
        val first = FakeSocket().also { api.sockets += it }
        val rt = runtime(dev.agentmesh.gen.agentmesh.v1.ActorClass.ACTOR_CLASS_DEVICE)
        show(rt)
        compose.waitUntil(5_000) { api.tickets.isNotEmpty() }
        live(first, input = false)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live")).fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodesWithTag("read-only").assertCountEquals(0)
        compose.onAllNodesWithTag("key-row").assertCountEquals(0)
        assertEquals(listOf(false), api.ticketInputs)
        // The keyboard opens a stream of its own, with the input right (the first is let go).
        val second = FakeSocket().also { api.sockets += it }
        compose.onNodeWithTag("keyboard-toggle").performClick()
        compose.waitUntil(5_000) { api.tickets.size >= 2 }
        assertEquals(listOf(false, true), api.ticketInputs)
        live(second, input = true)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live · typing")).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("key-row").assertIsDisplayed()
        // Keys go out as input frames, and the size is still not ours to set.
        compose.onNodeWithTag("key-Esc").performClick()
        compose.onNodeWithTag("key-↑").performScrollTo().performClick()
        compose.waitUntil(5_000) { second.sent.count { it.hasInput() } >= 2 }
        val typed = second.sent.filter { it.hasInput() }.map { it.input.data.toStringUtf8() }
        assertEquals(listOf("\u001B", "\u001B[A"), typed)
        assertEquals(0, second.sent.first().attach.cols)
        assertTrue(second.sent.none { it.hasResize() })
        // ^C is one tap, and does not need the sticky Ctrl.
        compose.onNodeWithTag("key-^C").performScrollTo().performClick()
        compose.waitUntil(5_000) { second.sent.count { it.hasInput() } >= 3 }
        assertEquals("\u0003", second.sent.filter { it.hasInput() }[2].input.data.toStringUtf8())
        // Ctrl is sticky for one key: Ctrl then "/" is sent as itself (no control code), and the next key has no Ctrl.
        compose.onNodeWithTag("key-ctrl").performScrollTo().performClick()
        compose.onNodeWithTag("key-→").performScrollTo().performClick()
        compose.onNodeWithTag("key-→").performScrollTo().performClick()
        runCatching { compose.waitUntil(2_000) { second.sent.count { it.hasInput() } >= 5 } }.onFailure { throw AssertionError("inputs so far: ${second.sent.filter { f -> f.hasInput() }.map { f -> f.input.data.toStringUtf8().replace("\u001B", "ESC") }}") }
        assertEquals(listOf("\u001B[1;5C", "\u001B[C"), second.sent.filter { it.hasInput() }.map { it.input.data.toStringUtf8() }.drop(3))
        // Hide puts the keyboard away: back to watching, with a ticket that asks for no input.
        compose.onNodeWithTag("key-hide").performScrollTo().performClick()
        compose.waitForIdle()
        compose.waitUntil(5_000) { api.tickets.size >= 3 }
        assertEquals(listOf(false, true, false), api.ticketInputs)
    }

    @Test fun `a host that did not let this device type says so, and the keys do nothing`() {
        val rt = runtime(dev.agentmesh.gen.agentmesh.v1.ActorClass.ACTOR_CLASS_DEVICE)
        val first = FakeSocket().also { api.sockets += it }
        show(rt)
        compose.waitUntil(5_000) { api.tickets.isNotEmpty() }
        live(first, input = false)
        val second = FakeSocket().also { api.sockets += it }
        compose.onNodeWithTag("keyboard-toggle").performClick()
        compose.waitUntil(5_000) { api.tickets.size >= 2 }
        live(second, input = false)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("The host didn't let this device type")).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("key-Esc").assertIsNotEnabled()
        assertTrue(second.sent.none { it.hasInput() })
    }

    @OptIn(androidx.compose.ui.test.ExperimentalTestApi::class)
    @Test fun `the keyboard's Enter and Backspace take the sticky Alt like every other key`() {
        val rt = runtime(dev.agentmesh.gen.agentmesh.v1.ActorClass.ACTOR_CLASS_DEVICE)
        val first = FakeSocket().also { api.sockets += it }
        show(rt)
        compose.waitUntil(5_000) { api.tickets.isNotEmpty() }
        live(first, input = false)
        val second = FakeSocket().also { api.sockets += it }
        compose.onNodeWithTag("keyboard-toggle").performClick()
        compose.waitUntil(5_000) { api.tickets.size >= 2 }
        live(second, input = true)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live · typing")).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("key-alt").performClick()
        compose.onNodeWithTag("ime-capture").performKeyInput { pressKey(androidx.compose.ui.input.key.Key.Enter) }
        compose.waitUntil(5_000) { second.sent.any { it.hasInput() } }
        assertEquals("\u001B\r", second.sent.first { it.hasInput() }.input.data.toStringUtf8())
        // The modifier was used up by that key.
        compose.onNodeWithTag("ime-capture").performKeyInput { pressKey(androidx.compose.ui.input.key.Key.Backspace) }
        compose.waitUntil(5_000) { second.sent.count { it.hasInput() } >= 2 }
        assertEquals("\u007f", second.sent.filter { it.hasInput() }[1].input.data.toStringUtf8())
    }

    @OptIn(androidx.compose.ui.test.ExperimentalTestApi::class)
    @Test fun `what the soft keyboard commits is typed once each, however fast they come, and Ctrl is used up by the first`() {
        val rt = runtime(dev.agentmesh.gen.agentmesh.v1.ActorClass.ACTOR_CLASS_DEVICE)
        val first = FakeSocket().also { api.sockets += it }
        show(rt)
        compose.waitUntil(5_000) { api.tickets.isNotEmpty() }
        live(first, input = false)
        val second = FakeSocket().also { api.sockets += it }
        compose.onNodeWithTag("keyboard-toggle").performClick()
        compose.waitUntil(5_000) { api.tickets.size >= 2 }
        live(second, input = true)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live · typing")).fetchSemanticsNodes().isNotEmpty() }
        fun typed() = second.sent.filter { it.hasInput() }.joinToString("") { it.input.data.toStringUtf8() }
        compose.onNodeWithTag("ime-capture").performTextInput("a")
        compose.onNodeWithTag("ime-capture").performTextInput("b")
        compose.waitUntil(5_000) { typed().length >= 2 }
        assertEquals("ab", typed())
        // Ctrl is one-shot over a commit of two characters: ^C, then d.
        compose.onNodeWithTag("key-ctrl").performClick()
        compose.onNodeWithTag("ime-capture").performTextInput("cd")
        compose.waitUntil(5_000) { typed().length >= 4 }
        assertEquals("ab\u0003d", typed())
    }

    @OptIn(androidx.compose.ui.test.ExperimentalTestApi::class)
    @Test fun `a long press selects after the screen was made again, and a tap that drops a selection is only that`() {
        val rt = runtime(dev.agentmesh.gen.agentmesh.v1.ActorClass.ACTOR_CLASS_DEVICE)
        val first = FakeSocket().also { api.sockets += it }
        show(rt)
        compose.waitUntil(5_000) { api.tickets.isNotEmpty() }
        live(first, input = false)
        compose.waitUntil(5_000) { compose.onAllNodes(hasContentDescription("fake claude", substring = true)).fetchSemanticsNodes().isNotEmpty() }
        val canvas = { compose.onNode(hasContentDescription("Terminal", substring = true)) }
        // The first touch starts the canvas's gesture handlers: they must follow what changes later.
        canvas().performTouchInput { longClick(androidx.compose.ui.geometry.Offset(30f, 10f)) }
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("copy").fetchSemanticsNodes().isNotEmpty() }
        // A tap drops it.
        canvas().performTouchInput { click(androidx.compose.ui.geometry.Offset(300f, 300f)) }
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("copy").fetchSemanticsNodes().isEmpty() }
        // The keyboard flips: the screen is made again (same screen, a new attach). Selecting still works.
        val second = FakeSocket().also { api.sockets += it }
        compose.onNodeWithTag("keyboard-toggle").performClick()
        compose.waitUntil(5_000) { api.tickets.size >= 2 }
        live(second, input = true)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live · typing")).fetchSemanticsNodes().isNotEmpty() }
        canvas().performTouchInput { longClick(androidx.compose.ui.geometry.Offset(30f, 10f)) }
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("copy").fetchSemanticsNodes().isNotEmpty() }
    }
}
