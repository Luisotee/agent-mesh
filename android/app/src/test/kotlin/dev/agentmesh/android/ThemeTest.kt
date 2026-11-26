package dev.agentmesh.android

import androidx.compose.ui.graphics.Color
import dev.agentmesh.android.net.toValue
import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.theme.ThemeSpec
import dev.agentmesh.android.theme.followedTheme
import dev.agentmesh.android.theme.hostTheme
import dev.agentmesh.gen.agentmesh.v1.Setting
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import dev.agentmesh.android.theme.Themes
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

class ThemeTest {
    private val dir = File("../../shared/themes")
    private val specs: List<ThemeSpec> get() = dir.listFiles { f -> f.extension == "json" }!!.sorted().map { Themes.parse(it.readText()) }

    @Test fun `the six themes the plan names ship, Dracula among them`() {
        assertEquals(
            setOf("Dracula", "Alucard", "Catppuccin Mocha", "Catppuccin Latte", "Tokyo Night", "Nord"),
            specs.map { it.name }.toSet(),
        )
        assertEquals("Dracula", Themes.DEFAULT)
    }

    @Test fun `every theme has all tokens, resolves, and has 16 distinct-enough terminal colours`() {
        val required = Themes.parse(File(dir, "dracula.json").readText()).tokens.keys
        for (s in specs) {
            assertEquals("${s.name} lacks tokens", required, s.tokens.keys)
            val t = Themes.resolve(s)
            assertEquals(16, t.terminal.ansi.size)
            assertEquals(s.dark, Themes.isDark(t.scheme.background))
            assertNotEquals("${s.name}: text on background", t.scheme.background, t.scheme.onBackground)
        }
    }

    @Test fun `light themes draw ANSI black and white in colours of their own`() {
        for (s in specs.filter { !it.dark }) {
            val t = Themes.resolve(s).terminal
            assertNotEquals("${s.name} black is the background", t.background, t.ansi[0])
            assertNotEquals("${s.name} white is the text", t.foreground, t.ansi[7])
        }
    }

    @Test fun `Dracula keeps the desktop's colours`() {
        val t = Themes.resolve(Themes.parse(File(dir, "dracula.json").readText()))
        assertEquals(Color(0xFF282A36), t.scheme.background)
        assertEquals(Color(0xFFBD93F9), t.scheme.primary)
        assertEquals(Color(0xFF8BE9FD), t.states.working)
        assertEquals(Color(0xFFFF5555), t.terminal.ansi[1])
    }

    @Test fun `a token a file leaves out is derived from the others`() {
        val spec = ThemeSpec("Mini", true, mapOf("background" to "#101010", "foreground" to "#eeeeee", "primary" to "#ff00ff", "state-failed" to "#ff0000"))
        val t = Themes.resolve(spec)
        assertEquals(Color(0xFFFF00FF), t.scheme.tertiary)
        assertEquals(t.scheme.background, t.scheme.surfaceContainerLow)
        assertEquals(Color(0xFFFF0000), t.terminal.ansi[1])
        assertEquals(Color(0xFFFF00FF), t.states.working)
        assertTrue(t.terminal.ansi.all { it.alpha == 1f })
    }

    private fun themeSetting(name: String, isSet: Boolean) =
        Setting.newBuilder().setKey("app.theme").setValue(Json.parseToJsonElement("\"$name\"").toValue()).setIsSet(isSet).build()

    private fun host(name: String?, isSet: Boolean = true) =
        HostState(settings = if (name == null) emptyMap() else mapOf("app.theme" to themeSetting(name, isSet)))

    @Test fun `the host's app theme is read from its settings when the host set it`() {
        assertEquals("Nord", hostTheme(host("Nord")))
        assertEquals(null, hostTheme(host(null)))
        // A host that never set it reports its default as the effective value: not a choice to follow.
        assertEquals(null, hostTheme(host("Dracula", isSet = false)))
    }

    @Test fun `with several hosts the phone follows the first that set a theme, not the first that reports one`() {
        // Host A never set it (its effective value is the default), host B set Nord.
        assertEquals("Nord", followedTheme(listOf(host("Dracula", isSet = false), host("Nord"))))
        assertEquals("Alucard", followedTheme(listOf(host(null), host("Alucard"), host("Nord"))))
        assertEquals(null, followedTheme(listOf(host("Dracula", isSet = false), host(null))))
        assertEquals(null, followedTheme(emptyList()))
    }

    @Test fun `a theme's own bright colours are read, and the rest are the normal ones lightened`() {
        val t = Themes.resolve(Themes.parse(File(dir, "dracula.json").readText()))
        assertEquals(Color(0xFFFF6E6E), t.terminal.ansi[9]) // Dracula's brightRed, as on the desktop
        assertEquals(Color(0xFFFFFFFF), t.terminal.ansi[15])
        val mini = Themes.resolve(ThemeSpec("Mini", true, mapOf("background" to "#101010", "foreground" to "#eeeeee", "primary" to "#ff00ff", "state-failed" to "#ff0000"), terminal = mapOf("red" to "#c00000", "brightGreen" to "#00ff00")))
        assertEquals(Color(0xFF00FF00), mini.terminal.ansi[10]) // listed
        assertNotEquals(mini.terminal.ansi[1], mini.terminal.ansi[9]) // not listed: lightened
    }

    @Test fun `every theme file names fonts and a density`() {
        for (f in dir.listFiles { f -> f.extension == "json" }!!) {
            val o = Json.parseToJsonElement(f.readText()).jsonObject
            assertTrue("${f.name} font", o["font"]!!.jsonObject.let { it["sans"] != null && it["mono"] != null })
            assertTrue("${f.name} density", o["density"]!!.jsonPrimitive.content in setOf("compact", "comfortable"))
        }
    }

    @Test fun `colours parse with and without alpha and reject nonsense`() {
        assertEquals(Color(0xFF123456), Themes.color("#123456"))
        assertEquals(Color(0x80123456), Themes.color("#80123456"))
        assertTrue(runCatching { Themes.color("red") }.isFailure)
    }
}
