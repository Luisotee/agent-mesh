package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.navigation.NavHostController
import androidx.navigation.compose.rememberNavController
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.DeepLink
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AppNav
import dev.agentmesh.android.ui.LocalContainer
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi", application = TestApp::class)
class NavTest {
    @get:Rule val compose = createComposeRule()

    private val theme = Themes.resolve(Themes.parse(File("src/main/assets/themes/dracula.json").readText()))

    /** A tapped notification leaves hosts, the host and the agent on the stack, however often and from wherever it is tapped. */
    @Test fun `tapping notifications for the same agent again never stacks a second copy`() {
        NotificationsTest.api = RecordingApi()
        val container = (ApplicationProvider.getApplicationContext<Application>() as MeshApp).container
        val host = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        var nav: NavHostController? = null
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) {
                AgentMeshTheme(theme) { nav = rememberNavController(); AppNav(nav!!) }
            }
        }
        fun stack() = nav!!.currentBackStack.value.mapNotNull { it.destination.route }.filter { !it.startsWith("Nav") && it != "" }
        fun tap(tab: Tab) {
            container.deepLink.value = DeepLink(host.id, "w1", tab)
            compose.waitUntil(10_000) { container.deepLink.value == null }
            compose.waitForIdle()
        }
        tap(Tab.Messages)
        val first = stack()
        assertEquals(listOf("hosts", "host/{id}", "host/{id}/agent/{agent}?tab={tab}"), first)
        tap(Tab.Messages)
        tap(Tab.Overview)
        assertEquals(first, stack())
    }
}
