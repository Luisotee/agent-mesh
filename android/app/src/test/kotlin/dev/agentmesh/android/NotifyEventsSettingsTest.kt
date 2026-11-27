package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertIsDisplayed
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
        // Why the last one can't be turned off is said.
        compose.onNodeWithTag("events-note").performScrollTo().assertIsDisplayed()
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
        // Registrations come from the endpoint and from the connection: what is checked is what the host was last sent.
        waitUntil { api.pushEvents.isNotEmpty() && api.pushEvents.last().isEmpty() }
        container.prefs.setNotifyEvents(setOf("plan", "limited"))
        waitUntil { api.pushEvents.lastOrNull() == listOf("limited", "plan") }
        container.prefs.setNotifyEvents(null)
        waitUntil { api.pushEvents.lastOrNull()?.isEmpty() == true && api.pushEvents.size > 2 && api.pushEvents.snapshot().any { it.isNotEmpty() } }
        assertEquals(emptyList<String>(), api.pushEvents.last())
    }

    private fun waitUntil(cond: () -> Boolean) {
        val deadline = System.currentTimeMillis() + 5_000
        while (!cond()) { check(System.currentTimeMillis() < deadline) { "timed out" }; org.robolectric.Shadows.shadowOf(android.os.Looper.getMainLooper()).idle(); Thread.sleep(20) }
    }

    @Test fun `a host that is slow to take the new list doesn't hold another host's back`() {
        val api = RecordingApi()
        NotificationsTest.api = api
        container.prefs.setNotifyEvents(null)
        val hosts = listOf("Home", "Work").map { name ->
            val host = container.hosts.add(name, listOf(Route.Direct("http://127.0.0.1:1")), "t")
            val deadline = System.currentTimeMillis() + 5_000
            var r: HostRuntime? = null
            while (r == null && System.currentTimeMillis() < deadline) { r = container.runtimes.value[host.id]; Thread.sleep(20) }
            r!!.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
            host
        }
        container.prefs.setPush(true)
        hosts.forEach { container.pushRegistrar.onEndpoint(it.id, SavedEndpoint("https://ntfy.example/${it.name}?up=1", "pub", "auth")) }
        waitUntil { api.registerCalls.get() >= 2 }
        val before = api.registerCalls.get()
        // Every registration hangs from here: both hosts must have begun theirs, not the second queued behind the first.
        api.registerGate = kotlinx.coroutines.CompletableDeferred()
        container.prefs.setNotifyEvents(setOf("plan"))
        waitUntil { api.registerCalls.get() >= before + 2 }
        api.registerGate!!.complete(Unit)
    }

    @Test fun `a quick run of changes leaves one registration waiting for a slow host, with the last list`() {
        val api = RecordingApi()
        NotificationsTest.api = api
        container.prefs.setNotifyEvents(null)
        val host = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val deadline = System.currentTimeMillis() + 5_000
        var rt: HostRuntime? = null
        while (rt == null && System.currentTimeMillis() < deadline) { rt = container.runtimes.value[host.id]; Thread.sleep(20) }
        rt!!.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        container.prefs.setPush(true)
        container.pushRegistrar.onEndpoint(host.id, SavedEndpoint("https://ntfy.example/up?up=1", "pub", "auth"))
        waitUntil { api.pushEvents.isNotEmpty() }
        Thread.sleep(300)
        val calls = api.registerCalls.get()
        val sent = api.pushEvents.size
        api.registerGate = kotlinx.coroutines.CompletableDeferred()
        container.prefs.setNotifyEvents(setOf("plan"))
        waitUntil { api.registerCalls.get() > calls }
        // Two more changes while the first is on its way: only the last of them is to be sent after it.
        container.prefs.setNotifyEvents(setOf("plan", "input"))
        container.prefs.setNotifyEvents(setOf("plan", "input", "report"))
        Thread.sleep(300)
        api.registerGate!!.complete(Unit)
        waitUntil { api.pushEvents.size >= sent + 2 }
        Thread.sleep(300)
        // The one on its way, then the last list once: not one for each change.
        assertEquals(sent + 2, api.pushEvents.size)
        assertEquals(listOf("input", "plan", "report"), api.pushEvents.last())
    }
}
