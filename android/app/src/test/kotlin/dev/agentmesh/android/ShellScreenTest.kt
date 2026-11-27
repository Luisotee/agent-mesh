package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.ShellScreen
import dev.agentmesh.gen.agentmesh.term.v1.AttachMode
import dev.agentmesh.gen.agentmesh.v1.Actor
import dev.agentmesh.gen.agentmesh.v1.ActorClass
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.Shell
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

/** A project's shell as a screen: its terminal at once with the keyboard on, sized to the phone when the phone opened it. */
@RunWith(RobolectricTestRunner::class)
@org.robolectric.annotation.GraphicsMode(org.robolectric.annotation.GraphicsMode.Mode.NATIVE)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi")
class ShellScreenTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val api = RecordingApi()
    private val started = mutableListOf<HostRuntime>()
    @After fun stop() { started.forEach { it.stop() } }

    private fun runtime(actor: ActorClass = ActorClass.ACTOR_CLASS_DEVICE, shells: List<Shell> = listOf(Shell.newBuilder().setId("sh1").setRepo("/home/u/shop-api").build())): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined))
        rt.connection.start(); started += rt
        api.shells = shells
        rt.onState(ConnState.Ready(HelloResponse.newBuilder().setActor(Actor.newBuilder().setClass_(actor).setId("device-1")).build()))
        rt.onFrame(snapshot(emptyList(), emptyList()))
        return rt
    }

    private fun show(rt: HostRuntime, id: String = "sh1") {
        val container = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> api })
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { ShellScreen(rt, id, {}) } } }
    }

    private fun live(s: FakeSocket, input: Boolean = true) {
        s.host(dev.agentmesh.gen.agentmesh.term.v1.TermFrame.newBuilder().setAttached(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, offset = 7, cols = 40, rows = 10).attached.toBuilder().setInput(input)).build())
        s.host(Frames.screen("user@host:~/shop-api\$ ", offset = 7)); s.host(Frames.replayDone(7))
    }

    @Test fun `a shell the phone opened is attached with the phone's size, with its own ticket and the input right`() {
        val s = FakeSocket().also { api.sockets += it }
        val rt = runtime()
        rt.phoneShells.add("sh1")
        show(rt)
        compose.waitUntil(5_000) { api.shellTickets.isNotEmpty() }
        // A shell's own ticket, never an agent's; typing is asked for at once.
        assertEquals(listOf("sh1"), api.shellTickets); assertTrue(api.tickets.isEmpty())
        assertEquals(listOf(true), api.shellTicketInputs)
        compose.waitUntil(5_000) { s.sent.isNotEmpty() }
        val attach = s.sent.first().attach
        assertTrue("the attach asks for the view's columns (${attach.cols})", attach.cols in 20..200)
        assertTrue("and its rows (${attach.rows})", attach.rows >= 5)
        live(s)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live · typing")).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("key-row").assertIsDisplayed()
        compose.onNodeWithTag("shell-path").assertIsDisplayed()
        compose.onNodeWithText("Terminal in shop-api").assertIsDisplayed()
    }

    @Test fun `a shell of another client is attached at size 0, like an agent's terminal`() {
        val s = FakeSocket().also { api.sockets += it }
        val rt = runtime()
        show(rt)
        compose.waitUntil(5_000) { api.shellTickets.isNotEmpty() }
        compose.waitUntil(5_000) { s.sent.isNotEmpty() }
        assertEquals(0, s.sent.first().attach.cols); assertEquals(0, s.sent.first().attach.rows)
        live(s)
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("Live · typing")).fetchSemanticsNodes().isNotEmpty() }
        assertTrue(s.sent.none { it.hasResize() })
    }

    @Test fun `ending the terminal asks first, and Back leaves it running`() {
        val s = FakeSocket().also { api.sockets += it }
        val rt = runtime()
        show(rt)
        compose.waitUntil(5_000) { api.shellTickets.isNotEmpty() }
        live(s)
        compose.onNodeWithTag("shell-menu").performClick()
        compose.onNodeWithTag("shell-end").performClick()
        compose.onNodeWithText("End the terminal in shop-api?").assertIsDisplayed()
        assertTrue(api.closed.isEmpty())
        compose.onNodeWithTag("action-confirm").performClick()
        compose.waitUntil(5_000) { api.closed == listOf("sh1") }
    }

    @Test fun `a shell the host does not list says it has ended, and one still being listed shows its screen`() {
        val rt = runtime(shells = emptyList())
        show(rt, id = "gone")
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("shell-gone").fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodesWithTag("keyboard-toggle").assertCountEquals(0)
    }

    @Test fun `a read-only device watches a shell and has no end menu`() {
        val s = FakeSocket().also { api.sockets += it }
        val rt = runtime(actor = ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE)
        show(rt)
        compose.waitUntil(5_000) { api.shellTickets.isNotEmpty() }
        live(s, input = false)
        assertEquals(listOf(false), api.shellTicketInputs)
        compose.onAllNodesWithTag("shell-menu").assertCountEquals(0)
        compose.onNodeWithTag("read-only").assertIsDisplayed()
    }
}
