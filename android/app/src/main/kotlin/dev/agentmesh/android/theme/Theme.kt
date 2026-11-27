package dev.agentmesh.android.theme

import androidx.compose.material3.ColorScheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import dev.agentmesh.android.net.toJson
import dev.agentmesh.android.store.HostState
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

/**
 * A theme is one JSON file of semantic tokens (docs/apps.md, "Settings, search and keyboard"): surfaces, text,
 * accent, the five agent states, and the terminal's colours. The files are in `assets/themes/` and use the
 * desktop's token names, so a theme is data and a new one is a new file. A token a file leaves out is derived from
 * the others ([resolve]).
 */
@Serializable
data class ThemeSpec(
    val name: String,
    val dark: Boolean,
    val tokens: Map<String, String>,
    /** background, foreground, cursor, selection, the eight colours and their `bright` ones (`brightRed`…); what is left out is derived. */
    val terminal: Map<String, String> = emptyMap(),
    /** The desktop's fonts and density (the files are shared); Android keeps its own type and spacing. */
    val font: ThemeFont = ThemeFont(),
    val density: String = "compact",
)

@Serializable
data class ThemeFont(val sans: String = "Geist", val mono: String = "JetBrains Mono")

/** The agent states' colours, which Material's scheme has no place for. */
data class StateColors(
    val working: Color, val needs: Color, val idle: Color, val done: Color, val failed: Color, val unknown: Color,
)

/** The terminal's screen and its 16 ANSI colours (index 0..15). */
data class TerminalColors(val background: Color, val foreground: Color, val cursor: Color, val selection: Color, val ansi: List<Color>)

/** Each vendor's brand colour in this theme (`vendor-*`: the monochrome brands use the theme's text colour). */
data class VendorColors(val claude: Color, val gemini: Color, val openai: Color, val copilot: Color, val opencode: Color, val deepseek: Color)

/** How many identity hues a theme has (`hue-1`…`hue-8`): a project's colour is one of them (`ui/Hue.kt`). */
const val HUES = 8

/**
 * A resolved theme: Material's scheme, the agent states' colours, the terminal's, and what the desktop's look added
 * (the identity [hues] a project is drawn in, the [vendors]' brand colours, the needs-you [glow]).
 */
class MeshTheme(
    val name: String, val dark: Boolean, val scheme: ColorScheme, val states: StateColors, val terminal: TerminalColors,
    val hues: List<Color>, val vendors: VendorColors, val glow: Color,
)

object Themes {
    const val DEFAULT = "Dracula"
    private val json = Json { ignoreUnknownKeys = true }

    fun parse(text: String): ThemeSpec = json.decodeFromString(ThemeSpec.serializer(), text)

    fun color(hex: String): Color {
        val h = hex.trim().removePrefix("#")
        require(h.length == 6 || h.length == 8) { "not a colour: $hex" }
        val v = h.toLong(16)
        return if (h.length == 6) Color(0xFF000000 or v) else Color(v)
    }

    private fun lighten(c: Color, by: Float) = Color(c.red + (1 - c.red) * by, c.green + (1 - c.green) * by, c.blue + (1 - c.blue) * by, c.alpha)

    fun resolve(spec: ThemeSpec): MeshTheme {
        val t = spec.tokens
        fun tok(key: String, vararg fallbacks: String): Color {
            for (k in listOf(key) + fallbacks) t[k]?.let { return color(it) }
            error("theme ${spec.name} has no token $key")
        }
        val background = tok("background")
        val foreground = tok("foreground")
        val surface = tok("surface", "background")
        val card = tok("card", "surface", "background")
        val popover = tok("popover", "card", "surface", "background")
        val muted = tok("muted", "card", "surface", "background")
        val mutedFg = tok("muted-foreground", "foreground")
        val border = tok("border", "muted", "card", "surface", "background")
        val primary = tok("primary")
        val onPrimary = tok("primary-foreground", "background")
        val secondary = tok("secondary", "muted", "card", "surface", "background")
        val onSecondary = tok("secondary-foreground", "foreground")
        val accent = tok("accent", "secondary", "muted", "card", "surface", "background")
        val onAccent = tok("accent-foreground", "foreground")
        val error = tok("destructive", "state-failed", "primary")
        val onError = tok("destructive-foreground", "background")
        val scheme = if (spec.dark) {
            darkColorScheme(
                primary = primary, onPrimary = onPrimary, primaryContainer = accent, onPrimaryContainer = onAccent,
                secondary = secondary, onSecondary = onSecondary, secondaryContainer = secondary, onSecondaryContainer = onSecondary,
                tertiary = primary, onTertiary = onPrimary, tertiaryContainer = accent, onTertiaryContainer = onAccent,
                background = background, onBackground = foreground, surface = background, onSurface = foreground,
                surfaceVariant = muted, onSurfaceVariant = mutedFg, inverseSurface = foreground, inverseOnSurface = background,
                error = error, onError = onError, errorContainer = error, onErrorContainer = onError,
                outline = border, outlineVariant = border,
                surfaceBright = popover, surfaceDim = surface, surfaceContainerLowest = surface, surfaceContainerLow = surface,
                surfaceContainer = card, surfaceContainerHigh = popover, surfaceContainerHighest = muted,
            )
        } else {
            lightColorScheme(
                primary = primary, onPrimary = onPrimary, primaryContainer = accent, onPrimaryContainer = onAccent,
                secondary = secondary, onSecondary = onSecondary, secondaryContainer = secondary, onSecondaryContainer = onSecondary,
                tertiary = primary, onTertiary = onPrimary, tertiaryContainer = accent, onTertiaryContainer = onAccent,
                background = background, onBackground = foreground, surface = background, onSurface = foreground,
                surfaceVariant = muted, onSurfaceVariant = mutedFg, inverseSurface = foreground, inverseOnSurface = background,
                error = error, onError = onError, errorContainer = error, onErrorContainer = onError,
                outline = border, outlineVariant = border,
                surfaceBright = popover, surfaceDim = surface, surfaceContainerLowest = surface, surfaceContainerLow = surface,
                surfaceContainer = card, surfaceContainerHigh = popover, surfaceContainerHighest = muted,
            )
        }
        val states = StateColors(
            working = tok("state-working", "primary"), needs = tok("state-needs", "primary"), idle = tok("state-idle", "muted-foreground", "foreground"),
            done = tok("state-done", "primary"), failed = tok("state-failed", "destructive", "primary"), unknown = tok("state-unknown", "muted-foreground", "foreground"),
        )
        val term = spec.terminal
        fun tc(key: String, fallback: Color) = term[key]?.let(::color) ?: fallback
        val red = tc("red", states.failed)
        val green = tc("green", states.done)
        val yellow = tc("yellow", states.unknown)
        val blue = tc("blue", primary)
        val magenta = tc("magenta", primary)
        val cyan = tc("cyan", states.working)
        val tBg = tc("background", surface)
        val tFg = tc("foreground", foreground)
        val normal = listOf(tc("black", tBg), red, green, yellow, blue, magenta, cyan, tc("white", tFg))
        // A theme's own bright colours when it lists them (the desktop reads the same keys), else the normal one lightened.
        val names = listOf("Black", "Red", "Green", "Yellow", "Blue", "Magenta", "Cyan", "White")
        val bright = names.mapIndexed { i, n -> tc("bright$n", if (i == 0) states.idle else lighten(normal[i], 0.2f)) }
        val terminal = TerminalColors(tBg, tFg, tc("cursor", tFg), tc("selection", border), normal + bright)
        val hues = List(HUES) { tok("hue-${it + 1}", "primary") }
        val vendors = VendorColors(
            tok("vendor-claude", "primary"), tok("vendor-gemini", "primary"), tok("vendor-openai", "foreground"),
            tok("vendor-copilot", "foreground"), tok("vendor-opencode", "foreground"), tok("vendor-deepseek", "primary"),
        )
        return MeshTheme(spec.name, spec.dark, scheme, states, terminal, hues, vendors, tok("glow", "state-needs", "primary"))
    }

    /** Whether a theme is dark, judged by its background, for a file that doesn't say. */
    fun isDark(background: Color) = background.luminance() < 0.5f
}

/**
 * The theme a host names with its `app.theme` setting, when it has set one itself: a setting's value is the effective
 * one, so a host that never set it reports its default ("Dracula"), which is not a choice to follow.
 */
fun hostTheme(s: HostState): String? =
    s.settings["app.theme"]?.takeIf { it.isSet }?.value?.toJson()?.let { it as? JsonPrimitive }?.takeIf { it.isString }?.content?.takeIf { it.isNotBlank() }

/** The theme the phone follows with several hosts: the first host, in the list's order, that has set one. */
fun followedTheme(states: List<HostState>): String? = states.firstNotNullOfOrNull { hostTheme(it) }
