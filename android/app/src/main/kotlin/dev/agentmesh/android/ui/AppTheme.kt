package dev.agentmesh.android.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.graphics.toArgb
import dev.agentmesh.android.AppContainer
import dev.agentmesh.android.theme.MeshTheme
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.theme.StateColors
import dev.agentmesh.android.theme.ThemeSpec

val LocalContainer = staticCompositionLocalOf<AppContainer> { error("no AppContainer") }
val LocalMeshTheme = staticCompositionLocalOf<MeshTheme> { error("no theme") }

/** The agent states' colours of the theme in use. */
val stateColors: StateColors @Composable get() = LocalMeshTheme.current.states

@Composable
fun AgentMeshTheme(theme: MeshTheme, content: @Composable () -> Unit) {
    MatchWindow(theme)
    CompositionLocalProvider(LocalMeshTheme provides theme, LocalReducedMotion provides rememberReducedMotion()) {
        // Springs for what answers a touch (sheets, menus, chips); the scheme is the expressive one.
        MaterialTheme(colorScheme = theme.scheme, typography = MeshTypography, motionScheme = androidx.compose.material3.MotionScheme.expressive(), content = content)
    }
}

/**
 * The window follows the theme, not the system's mode: the status and navigation bars' icons are dark on a light theme and light
 * on a dark one (edge-to-edge draws the app under them, and the system's own choice follows the system's mode: a light theme on
 * a dark phone had white icons on a light bar), and the window behind the app is the theme's background (what shows before the
 * first frame, when the keyboard resizes the window and on an overscroll).
 */
@Composable
private fun MatchWindow(theme: MeshTheme) {
    val view = androidx.compose.ui.platform.LocalView.current
    if (view.isInEditMode) return
    androidx.compose.runtime.SideEffect {
        val window = (view.context as? android.app.Activity)?.window ?: return@SideEffect
        window.setBackgroundDrawable(android.graphics.drawable.ColorDrawable(theme.scheme.background.toArgb()))
        androidx.core.view.WindowCompat.getInsetsController(window, view).apply {
            isAppearanceLightStatusBars = !theme.dark
            isAppearanceLightNavigationBars = !theme.dark
        }
    }
}

/** The theme this device chose (by default the host's), resolved against the system's dark mode. */
@Composable
fun rememberTheme(container: AppContainer): MeshTheme {
    val choice = container.themes.choice.collectAsStateWithLifecycleCompat()
    val host = container.themes.host.collectAsStateWithLifecycleCompat()
    val dark = isSystemInDarkTheme()
    return androidx.compose.runtime.remember(choice.value, dark, host.value) { container.themes.resolve(choice.value, dark, host.value) }
}


/** For previews and tests: a theme straight from a spec. */
fun themeOf(spec: ThemeSpec): MeshTheme = Themes.resolve(spec)
