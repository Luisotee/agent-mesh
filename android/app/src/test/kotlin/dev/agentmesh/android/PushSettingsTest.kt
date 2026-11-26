package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performScrollTo
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.settings.DeviceSettingsScreen
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** The device settings' push row: off, no distributor installed, and on with each host's state. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h1600dp-xxhdpi", application = TestApp::class)
class PushSettingsTest {
    @get:Rule val compose = createComposeRule()

    private val theme = Themes.resolve(Themes.parse(File("src/main/assets/themes/dracula.json").readText()))
    private val container get() = (ApplicationProvider.getApplicationContext<Application>() as MeshApp).container

    private fun show() = compose.setContent {
        CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { DeviceSettingsScreen(onBack = {}) } }
    }

    @Test fun `without a distributor the row says to install ntfy and can't be turned on`() {
        NotificationsTest.api = RecordingApi()
        (container.push as FakePushChannel).installed = emptyList()
        show()
        compose.onNodeWithText("install ntfy", substring = true).performScrollTo()
        compose.onNodeWithTag("push-switch").assertIsNotEnabled()
    }

    @Test fun `on, it names the distributor and how each host stands, with a test button`() {
        NotificationsTest.api = RecordingApi()
        val channel = container.push as FakePushChannel
        channel.chosen = "io.heckel.ntfy"
        val host = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        container.prefs.setPush(true)
        container.pushRegistrar.failed(host.id, "The distributor couldn't reach its server")
        show()
        compose.onNodeWithText("Through io.heckel.ntfy", substring = true).performScrollTo()
        compose.onNodeWithText("Home: The distributor couldn't reach its server").performScrollTo()
        compose.onNodeWithText("Send a test notification").performScrollTo()
    }
}
