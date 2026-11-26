package dev.agentmesh.android.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Typography
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.sp
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
    CompositionLocalProvider(LocalMeshTheme provides theme) {
        MaterialTheme(colorScheme = theme.scheme, typography = Typography(), content = content)
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

val Mono = FontFamily.Monospace
val MonoSmall = TextStyle(fontFamily = FontFamily.Monospace, fontSize = 12.sp)

/** For previews and tests: a theme straight from a spec. */
fun themeOf(spec: ThemeSpec): MeshTheme = Themes.resolve(spec)
