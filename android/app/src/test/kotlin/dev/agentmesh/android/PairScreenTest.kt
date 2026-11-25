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
        PairTestApp.calls.clear()
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

    @Test fun `a second link opened while the screen is up replaces the first, and is taken once`() {
        show(initial = link)
        compose.onNode(hasText("work station", substring = true)).assertIsDisplayed()
        container.pairLink.value = link.replace("hn=work+station", "hn=other+box")
        compose.waitUntil(5_000) { compose.onAllNodes(hasText("other box", substring = true)).fetchSemanticsNodes().isNotEmpty() }
        assertEquals(null, container.pairLink.value)
    }

    @Test fun `a link that comes in while a pairing is running waits, and is shown after it instead of being wiped`() {
        var paired: String? = null
        val gate = kotlinx.coroutines.CompletableDeferred<Unit>()
        show(initial = link, onPaired = { paired = it })
        PairTestApp.gate = gate
        try {
            compose.onNodeWithTag("pair").performClick()
            compose.waitUntil(5_000) { PairTestApp.calls.isNotEmpty() }
            container.pairLink.value = link.replace("hn=work+station", "hn=other+box")
            compose.waitForIdle()
            // Mid-call: the field is still the first code, and the second one is held.
            compose.onNode(hasText("work station", substring = true)).assertIsDisplayed()
            assertTrue(container.pairLink.value != null)
            compose.onNodeWithTag("scan").assertIsNotEnabled()
            gate.complete(Unit)
            compose.waitUntil(5_000) { compose.onAllNodesWithTag("paired-notice").fetchSemanticsNodes().isNotEmpty() }
            compose.onNode(hasText("other box", substring = true)).assertIsDisplayed()
            assertEquals("the screen stays for the second code", null, paired)
            assertEquals(null, container.pairLink.value)
        } finally {
            PairTestApp.gate = null
        }
    }

    @Test fun `pairing a host again reconnects it with its new token and routes`() {
        val known = container.hosts.add("Home", listOf(dev.agentmesh.android.net.Route.Direct("https://old:1", fp)), "amt_old", daemonId = "h1234567890ab")
        val rt = run {
            val deadline = System.currentTimeMillis() + 5_000
            while (container.runtimes.value[known.id] == null && System.currentTimeMillis() < deadline) Thread.sleep(20)
            container.runtimes.value[known.id]!!
        }
        rt.onState(dev.agentmesh.android.store.ConnState.Stopped(dev.agentmesh.android.store.ConnState.Reason.Auth, "the host refused this device's token"))
        var paired: String? = null
        show(initial = link, onPaired = { paired = it })
        compose.onNodeWithTag("pair").performClick()
        compose.waitUntil(5_000) { paired != null }
        assertEquals(known.id, paired)
        assertEquals("amt_device", container.hosts.token(known.id))
        compose.waitUntil(5_000) { rt.state.value.conn !is dev.agentmesh.android.store.ConnState.Stopped }
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
                    PairTestApp.gate?.await()
                    return PairTestApp.redeem(token, deviceName)
                }
            }
        },
    )

    companion object {
        val calls = mutableListOf<String>()
        /** When set, a redeem waits for it: a pairing that is still running. */
        @Volatile var gate: kotlinx.coroutines.CompletableDeferred<Unit>? = null
        var redeem: (String, String) -> Redeemed = { _, _ -> error("no host") }
    }
}
