package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.onNodeWithTag
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

    private val link = "agentmesh://pair?v=1&h=h1234567890ab&fp=sha256:${"ab".repeat(32)}&t=amp_secret&d=https%3A%2F%2F100.64.0.1%3A9443"

    @Test fun `a pairing link opened from outside lands on the pairing screen with the link in its field`() {
        val container = (ApplicationProvider.getApplicationContext<Application>() as MeshApp).container
        var nav: NavHostController? = null
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) {
                AgentMeshTheme(theme) { nav = rememberNavController(); AppNav(nav!!) }
            }
        }
        container.pairLink.value = link
        compose.waitUntil(10_000) { nav!!.currentDestination?.route == "pair" }
        compose.waitForIdle()
        // The screen took the link (once), and shows the host for the user to confirm; nothing was sent.
        assertEquals(null, container.pairLink.value)
        compose.onNodeWithTag("summary").assertIsDisplayed()
    }

    @Test fun `an agentmesh pair link in an activity's intent is handed to the app, and never acted on by itself`() {
        val container = (ApplicationProvider.getApplicationContext<Application>() as MeshApp).container
        container.pairLink.value = null
        val intent = android.content.Intent(android.content.Intent.ACTION_VIEW, android.net.Uri.parse(link))
        org.robolectric.Robolectric.buildActivity(dev.agentmesh.android.MainActivity::class.java, intent).create().get()
        assertEquals(link, container.pairLink.value)
        assertEquals("no host was added by opening a link", 0, container.hosts.hosts.value.size)
        // Another scheme or host isn't ours.
        container.pairLink.value = null
        org.robolectric.Robolectric.buildActivity(dev.agentmesh.android.MainActivity::class.java, android.content.Intent(android.content.Intent.ACTION_VIEW, android.net.Uri.parse("https://pair/?x=1"))).create().get()
        assertEquals(null, container.pairLink.value)
    }
}
