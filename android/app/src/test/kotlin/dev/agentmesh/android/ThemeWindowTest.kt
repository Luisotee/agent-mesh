package dev.agentmesh.android

import androidx.activity.ComponentActivity
import androidx.compose.material3.Text
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.core.view.WindowCompat
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** The window follows the app's theme, not the system's mode: bar icons that can be seen, and the right colour behind the app. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36])
class ThemeWindowTest {
    @get:Rule val compose = createAndroidComposeRule<ComponentActivity>()
    private fun theme(file: String) = Themes.resolve(Themes.parse(File("../../shared/themes/$file.json").readText()))

    @Test fun `the window behind the app and the bars' icons follow the theme, light or dark`() {
        val light = theme("catppuccin-latte")
        val dark = theme("dracula")
        var current by mutableStateOf(light)
        compose.setContent { AgentMeshTheme(current) { Text("x") } }
        compose.waitForIdle()
        fun window() = compose.activity.window
        fun lightIcons() = WindowCompat.getInsetsController(window(), window().decorView).let { it.isAppearanceLightStatusBars to it.isAppearanceLightNavigationBars }
        assertEquals(light.scheme.background.toArgb(), (window().decorView.background as android.graphics.drawable.ColorDrawable).color)
        // A light theme needs dark icons.
        assertEquals(true to true, lightIcons())
        current = dark
        compose.waitForIdle()
        assertEquals(dark.scheme.background.toArgb(), (window().decorView.background as android.graphics.drawable.ColorDrawable).color)
        assertEquals(false to false, lightIcons())
        assertTrue(light.scheme.background.toArgb() != dark.scheme.background.toArgb())
        assertFalse(light.dark)
    }
}
