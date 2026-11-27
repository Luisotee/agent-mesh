package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AllHostsDockScreen
import dev.agentmesh.android.ui.HostsScreen
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** The live dock across hosts: every host's tiles with its name on them, and a way in from the hosts screen. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w411dp-h1600dp-xxhdpi")
class AllHostsDockTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val app get() = ApplicationProvider.getApplicationContext<Application>()
    private val container get() = (app as MeshApp).container

    private fun host(name: String, ready: Boolean, vararg agents: dev.agentmesh.gen.agentmesh.v1.Agent): HostRuntime {
        val h = container.hosts.add(name, listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val deadline = System.currentTimeMillis() + 5_000
        var rt: HostRuntime? = null
        while (rt == null && System.currentTimeMillis() < deadline) { rt = container.runtimes.value[h.id]; Thread.sleep(20) }
        rt!!
        if (ready) rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1", "Run of $name")), agents.toList()))
        return rt
    }

    @Test fun `every host's live agents are on it, with the host's name, and a tap opens the agent on its host`() {
        NotificationsTest.api = RecordingApi()
        val home = host("Home", true, agent("a1", name = "home-fix", state = AgentState.AGENT_STATE_WORKING, detail = "Bash"))
        val work = host("Work", true, agent("b1", name = "work-ask", state = AgentState.AGENT_STATE_NEEDS_INPUT, needsInput = 1, detail = "Edit"))
        var opened: Triple<String, String, Tab>? = null
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AllHostsDockScreen({}) { h, a, t -> opened = Triple(h, a, t) } } }
        }
        compose.onAllNodesWithTag("tile").assertCountEquals(2)
        compose.onNodeWithText("Working: Bash").assertIsDisplayed()
        compose.onNodeWithText("Work · Run of Work", substring = true).assertIsDisplayed()
        compose.onNodeWithText("Home · Run of Home", substring = true).assertIsDisplayed()
        compose.onAllNodesWithTag("dock-unreachable").assertCountEquals(0)
        // The one that needs you is first, and opens its terminal on its own host.
        compose.onNodeWithText("work-ask").performClick()
        assertEquals(Triple(work.hostId, "b1", Tab.Terminal), opened)
        compose.onNodeWithText("home-fix").performClick()
        assertEquals(Triple(home.hostId, "a1", Tab.Chat), opened)
    }

    @Test fun `a host that is not connected is counted above the tiles`() {
        NotificationsTest.api = RecordingApi()
        host("Home", true, agent("a1", name = "home-fix", state = AgentState.AGENT_STATE_WORKING))
        host("Away", false, agent("c1", name = "away-fix", state = AgentState.AGENT_STATE_WORKING))
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AllHostsDockScreen({}) { _, _, _ -> } } }
        }
        compose.onNodeWithTag("dock-unreachable").assertIsDisplayed()
        compose.onNodeWithText("1 of 2 hosts not connected", substring = true).assertIsDisplayed()
    }

    @Test fun `the hosts screen has the way in once there is a host`() {
        NotificationsTest.api = RecordingApi()
        host("Home", true, agent("a1", name = "home-fix", state = AgentState.AGENT_STATE_WORKING))
        var live = 0
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { HostsScreen({}, {}, {}, {}, {}, onLive = { live++ }) } }
        }
        compose.onNodeWithTag("live-all").performClick()
        assertEquals(1, live)
    }
}
