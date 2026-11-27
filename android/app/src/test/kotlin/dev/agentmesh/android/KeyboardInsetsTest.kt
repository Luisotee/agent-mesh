package dev.agentmesh.android

import android.app.Application
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.test.captureToImage
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onRoot
import androidx.core.graphics.Insets
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AgentScreen
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.TerminalCanvas
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode
import java.io.File

/**
 * What no emulator is here to show (`android/CLAUDE.md`): the screen laid out with the window's insets of a phone with a
 * navigation bar and its keyboard open, and the terminal's first frame before the host has answered. Both draw real pixels
 * and real layout under Robolectric's native graphics.
 */
@RunWith(RobolectricTestRunner::class)
@GraphicsMode(GraphicsMode.Mode.NATIVE)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi")
class KeyboardInsetsTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val api = RecordingApi()
    private val started = mutableListOf<HostRuntime>()

    @org.junit.After fun stop() { started.forEach { it.stop() } }

    private fun runtime(): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined))
        rt.start()
        started += rt
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1")), listOf(agent("a1", name = "shop-fix", state = AgentState.AGENT_STATE_IDLE).toBuilder().setKind("claude").setTerminalId("t-a1").build())))
        return rt
    }

    /** The window's insets as a phone with a navigation bar of [nav] px, and a keyboard of [ime] px above it (0: closed), as the system reports them: the keyboard's inset includes the bar. */
    private fun insets(view: android.view.View, nav: Int, ime: Int) {
        compose.runOnUiThread {
            val b = WindowInsetsCompat.Builder()
                .setInsets(WindowInsetsCompat.Type.statusBars(), Insets.of(0, 60, 0, 0))
                .setInsets(WindowInsetsCompat.Type.navigationBars(), Insets.of(0, 0, 0, nav))
            if (ime > 0) b.setInsets(WindowInsetsCompat.Type.ime(), Insets.of(0, 0, 0, ime + nav)).setVisible(WindowInsetsCompat.Type.ime(), true)
            ViewCompat.dispatchApplyWindowInsets(view, b.build())
        }
        compose.waitForIdle()
    }

    @Test fun `the composer sits on the keyboard as close as it sits on the navigation bar`() {
        val rt = runtime()
        val container = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> api })
        var view: android.view.View? = null
        compose.setContent { view = LocalView.current; CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AgentScreen(rt, "a1", {}, Tab.Chat) } } }
        compose.waitForIdle()
        val rootH = compose.onRoot().fetchSemanticsNode().boundsInRoot.height
        val nav = 126
        val ime = 800
        insets(view!!, nav, 0)
        val closed = compose.onNodeWithTag("prompt").fetchSemanticsNode().boundsInRoot.bottom
        insets(view!!, nav, ime)
        val open = compose.onNodeWithTag("prompt").fetchSemanticsNode().boundsInRoot.bottom
        // The keyboard's top is the window's bottom less its inset (which includes the bar); the field is as far above it as above the bar with no keyboard.
        val gapClosed = rootH - nav - closed
        val gapOpen = rootH - (ime + nav) - open
        assertEquals("the composer is $gapOpen px above the keyboard, $gapClosed px above the bar when it is closed", gapClosed, gapOpen, 1f)
    }

    @Test fun `a terminal that is empty at its first frame is drawn when the host's screen arrives`() {
        val screen = TermScreen()
        compose.setContent { Box(Modifier.fillMaxSize()) { TerminalCanvas(screen, theme.terminal) } }
        compose.waitForIdle()
        // The first frame, before the host has answered: there is no emulator to draw.
        compose.onRoot().captureToImage()
        compose.runOnIdle {
            screen.reset(80, 24)
            screen.write("hello from the host\r\n\u001b[32mgreen\u001b[0m line\r\n".toByteArray())
        }
        compose.waitForIdle()
        val img = compose.onRoot().captureToImage().asAndroidBitmap()
        val bg = theme.terminal.background
        val bgArgb = android.graphics.Color.argb(255, (bg.red * 255 + 0.5f).toInt(), (bg.green * 255 + 0.5f).toInt(), (bg.blue * 255 + 0.5f).toInt())
        var onBg = 0
        var ink = 0
        for (y in 0 until img.height step 2) for (x in 0 until img.width step 2) {
            val p = img.getPixel(x, y)
            if (p == bgArgb) onBg++ else if (android.graphics.Color.alpha(p) > 0) ink++
        }
        val total = (img.height / 2 + 1) * (img.width / 2 + 1)
        assertTrue("the canvas was not painted: only $onBg of $total sampled pixels are the terminal's background", onBg > total * 0.8)
        assertTrue("no text was drawn ($ink pixels)", ink > 100)
    }
}
