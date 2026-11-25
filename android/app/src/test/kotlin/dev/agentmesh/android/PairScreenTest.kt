package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.assertIsEnabled
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performTextInput
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.hosts.PairingClients
import dev.agentmesh.android.net.PairingApi
import dev.agentmesh.android.net.Redeemed
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.PairScreen
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi", application = PairTestApp::class)
class PairScreenTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("src/main/assets/themes/dracula.json").readText()))
    private val container get() = (ApplicationProvider.getApplicationContext<Application>() as MeshApp).container

    private val fp = "ab".repeat(32)
    private val link = "agentmesh://pair?v=1&h=h1234567890ab&fp=sha256:$fp&t=amp_secret&hn=work+station&rtcp=relay.example.com%3A8443&rt=amr_c&d=https%3A%2F%2F100.64.0.1%3A9443"

    private fun show(initial: String? = null, onPaired: (String) -> Unit = {}) {
        PairTestApp.redeem = { _, name -> Redeemed("dev1", name, "amt_device", "h1234567890ab", "work station", "sha256:$fp") }
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { PairScreen(initial, {}, onPaired) } }
        }
    }

    @Test fun `nothing can be paired until a code that reads is entered, and a bad one says why`() {
        show()
        compose.onNodeWithTag("pair").assertIsNotEnabled()
        compose.onNodeWithTag("link").performTextInput("not a link")
        compose.onNodeWithTag("link-error").assertIsDisplayed()
        compose.onNodeWithTag("pair").assertIsNotEnabled()
        compose.onAllNodesWithTag("summary").assertCountEquals(0)
    }

    @Test fun `a link opened from outside is shown for confirmation and not used until Pair is tapped`() {
        PairTestApp.calls.clear()
        show(initial = link)
        compose.onNodeWithTag("summary").assertIsDisplayed()
        compose.onNode(hasText("Through your relay: relay.example.com:8443")).assertIsDisplayed()
        compose.onNode(hasText("Direct: https://100.64.0.1:9443")).assertIsDisplayed()
        compose.onNode(hasText("work station", substring = true)).assertIsDisplayed()
        compose.onNodeWithTag("pair").assertIsEnabled()
        assertTrue("nothing was sent before the tap", PairTestApp.calls.isEmpty())
    }

    @Test fun `Pair redeems the code, saves the host with its routes and token, and clears the link`() {
        var paired: String? = null
        show(initial = link, onPaired = { paired = it })
        compose.onNodeWithTag("pair").performClick()
        compose.waitUntil(5_000) { paired != null }
        val host = container.hosts.get(paired!!)!!
        assertEquals("work station", host.name); assertEquals("h1234567890ab", host.daemonId); assertEquals(2, host.routes.size)
        assertEquals("amt_device", container.hosts.token(host.id))
        assertEquals(1, PairTestApp.calls.count { it == "direct" })
    }

    @Test fun `a code the host refuses shows its reason and saves nothing`() {
        show(initial = link)
        PairTestApp.redeem = { _, _ -> throw dev.agentmesh.android.net.ApiException(com.connectrpc.Code.UNAUTHENTICATED, "pairing token already used") }
        val before = container.hosts.hosts.value.size
        compose.onNodeWithTag("pair").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("pair-error").fetchSemanticsNodes().isNotEmpty() }
        compose.onNode(hasText("already used", substring = true)).assertIsDisplayed()
        assertEquals(before, container.hosts.hosts.value.size)
        compose.onNodeWithTag("pair").assertIsEnabled()
        assertFalse(container.hosts.hosts.value.any { it.name == "work station" && it.daemonId == null })
    }
}

/** The app with a host that answers pairing from a lambda: what the screen does with each answer is checked end to end. */
class PairTestApp : MeshApp() {
    override fun createContainer(): AppContainer = AppContainer(
        this, Plain, newApi = { _, _ -> RecordingApi() },
        pairingClients = PairingClients { route ->
            object : PairingApi {
                override suspend fun redeem(token: String, deviceName: String): Redeemed {
                    calls += if (route is dev.agentmesh.android.net.Route.Direct) "direct" else "relay"
                    return PairTestApp.redeem(token, deviceName)
                }
            }
        },
    )

    companion object {
        val calls = mutableListOf<String>()
        var redeem: (String, String) -> Redeemed = { _, _ -> error("no host") }
    }
}
