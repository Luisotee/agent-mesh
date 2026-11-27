package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.assertIsOff
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performScrollTo
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.SavedEndpoint
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.settings.DeviceSettingsScreen
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.Rule
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** The device's own choice of what to be told of: the settings rows, and what the host is sent with each registration. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h1600dp-xxhdpi", application = TestApp::class)
class NotifyEventsSettingsTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val container get() = (ApplicationProvider.getApplicationContext<Application>() as MeshApp).container

    private fun show() = compose.setContent {
        CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { DeviceSettingsScreen(onBack = {}) } }
    }

    @Test fun `the phone follows each host until it chooses, and the last kind can't be turned off`() {
        NotificationsTest.api = RecordingApi()
        container.prefs.setNotifyEvents(null)
        show()
        compose.onNodeWithTag("choose-events").performScrollTo().assertIsOff()
        assertNull(container.prefs.notifyEvents.value)
        compose.onNodeWithTag("choose-events").performClick()
        assertEquals(setOf("input", "plan", "question", "report", "failed", "error"), container.prefs.notifyEvents.value)
        // The usage-limit kind is on request, as on the desktop.
        compose.onNodeWithTag("event-limited").performScrollTo().assertIsOff().performClick()
        assertTrue("limited" in container.prefs.notifyEvents.value!!)
        // Turn every kind off but one: the last stays on (none at all is the host's own off switch).
        for (k in listOf("input", "plan", "question", "report", "failed", "error", "limited").dropLast(1)) compose.onNodeWithTag("event-$k").performScrollTo().performClick()
        assertEquals(setOf("limited"), container.prefs.notifyEvents.value)
        compose.onNodeWithTag("event-limited").performScrollTo().assertIsNotEnabled()
        // Back to following the hosts.
        compose.onNodeWithTag("choose-events").performScrollTo().performClick()
        assertNull(container.prefs.notifyEvents.value)
    }

    @Test fun `the host is sent the phone's list with its registration, and again when the choice changes`() {
        val api = RecordingApi()
        NotificationsTest.api = api
        container.prefs.setNotifyEvents(null)
        val host = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = run {
            var r: HostRuntime? = null
            val deadline = System.currentTimeMillis() + 5_000
            while (r == null && System.currentTimeMillis() < deadline) { r = container.runtimes.value[host.id]; Thread.sleep(20) }
            r!!
        }
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        container.prefs.setPush(true)
        container.pushRegistrar.onEndpoint(host.id, SavedEndpoint("https://ntfy.example/up?up=1", "pub", "auth"))
        waitUntil { api.pushEvents.size >= 1 }
        assertEquals(emptyList<String>(), api.pushEvents.last())
        container.prefs.setNotifyEvents(setOf("plan", "limited"))
        waitUntil { api.pushEvents.size >= 2 }
        assertEquals(listOf("limited", "plan"), api.pushEvents.last())
        container.prefs.setNotifyEvents(null)
        waitUntil { api.pushEvents.size >= 3 }
        assertEquals(emptyList<String>(), api.pushEvents.last())
    }

    private fun waitUntil(cond: () -> Boolean) {
        val deadline = System.currentTimeMillis() + 5_000
        while (!cond()) { check(System.currentTimeMillis() < deadline) { "timed out" }; org.robolectric.Shadows.shadowOf(android.os.Looper.getMainLooper()).idle(); Thread.sleep(20) }
    }
}
