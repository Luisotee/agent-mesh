package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.hasTestTag
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.assertIsNotDisplayed
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithContentDescription
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.navigation.NavHostController
import androidx.navigation.compose.rememberNavController
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AppNav
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** The drawer around the screens: always open beside them when wide, a slide-over below, and an agent from it replaces the open one. */
abstract class DrawerNavBase {
    @get:Rule val compose = createAndroidComposeRule<androidx.activity.ComponentActivity>()
    protected val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    protected val container get() = (ApplicationProvider.getApplicationContext<Application>() as MeshApp).container
    protected var nav: NavHostController? = null

    protected fun start() {
        NotificationsTest.api = RecordingApi()
        val h = container.hosts.add("wsl", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val deadline = System.currentTimeMillis() + 5_000
        while (container.runtimes.value[h.id]?.api() == null && System.currentTimeMillis() < deadline) Thread.sleep(20)
        val rt = container.runtimes.value.getValue(h.id)
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1", "Fix the typo")), listOf(
            agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead", state = AgentState.AGENT_STATE_WORKING),
            agent("w1", parent = "lead", name = "shop-fix", state = AgentState.AGENT_STATE_WORKING, at = 2_000),
            agent("w2", parent = "lead", name = "shop-docs", state = AgentState.AGENT_STATE_IDLE, at = 3_000),
        )))
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { nav = rememberNavController(); AppNav(nav!!) } }
        }
    }

    protected fun stack() = nav!!.currentBackStack.value.mapNotNull { it.destination.route }.filter { !it.startsWith("Nav") && it != "" }
    protected val agentRoute = "host/{id}/agent/{agent}?tab={tab}"
    protected fun open(tag: String) {
        compose.waitUntil(5_000) { compose.onAllNodesWithTag(tag).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag(tag).performClick()
        compose.waitForIdle()
    }
}

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w900dp-h900dp-xxhdpi")
class WideDrawerNavTest : DrawerNavBase() {
    @Test fun `the drawer stays open beside the screens, and an agent from it replaces the open one`() {
        start()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("sidebar").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("sidebar").assertIsDisplayed()
        // No menu button: the drawer is always there.
        assertEquals(0, compose.onAllNodesWithTag("open-drawer").fetchSemanticsNodes().size)
        open("worker-w1")
        assertEquals(listOf("home", agentRoute), stack())
        compose.onNodeWithTag("sidebar").assertIsDisplayed()
        open("worker-w2")
        // Replaced, not stacked: Back from any agent is home.
        assertEquals(listOf("home", agentRoute), stack())
        assertEquals("w2", nav!!.currentBackStackEntry!!.arguments!!.getString("agent"))
        // The drawer's title goes home: one home on the stack, not two.
        open("sidebar-title")
        assertEquals(listOf("home"), stack())
        open("sidebar-title")
        assertEquals(listOf("home"), stack())
    }

    @Test fun `settings use the whole window`() {
        start()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("sidebar").fetchSemanticsNodes().isNotEmpty() }
        open("open-settings")
        compose.waitUntil(5_000) { nav!!.currentDestination?.route == "settings" }
        compose.waitForIdle()
        assertTrue(compose.onAllNodes(hasTestTag("sidebar")).fetchSemanticsNodes().isEmpty())
        // Settings › Hosts is one tap in.
        open("settings-hosts")
        assertEquals(listOf("home", "settings", "hosts"), stack())
    }
}

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w411dp-h900dp-xxhdpi")
class NarrowDrawerNavTest : DrawerNavBase() {
    @Test fun `the menu button opens the drawer over home, and an agent from it goes home then agent`() {
        start()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("open-drawer").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithContentDescription("Menu").performClick()
        compose.waitForIdle()
        open("worker-w1")
        assertEquals(listOf("home", agentRoute), stack())
        // On the agent the menu opens it again and another agent replaces this one.
        compose.onNodeWithTag("open-drawer").performClick()
        compose.waitForIdle()
        open("worker-w2")
        assertEquals(listOf("home", agentRoute), stack())
        assertEquals("w2", nav!!.currentBackStackEntry!!.arguments!!.getString("agent"))
    }

    @Test fun `system Back closes an open drawer before it does anything else`() {
        start()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("open-drawer").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithContentDescription("Menu").performClick()
        compose.waitForIdle()
        compose.onNodeWithTag("sidebar").assertIsDisplayed()
        compose.runOnUiThread { compose.activity.onBackPressedDispatcher.onBackPressed() }
        compose.waitForIdle()
        compose.onNodeWithTag("sidebar").assertIsNotDisplayed()
        assertEquals(listOf("home"), stack())
    }
}
