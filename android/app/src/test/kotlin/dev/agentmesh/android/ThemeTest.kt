package dev.agentmesh.android

import androidx.compose.ui.graphics.Color
import dev.agentmesh.android.net.toValue
import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.theme.ThemeSpec
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

    @Test fun `the host's app theme is read from its settings and followed by default`() {
        val setting = Setting.newBuilder().setKey("app.theme").setValue(Json.parseToJsonElement("\"Nord\"").toValue()).build()
        assertEquals("Nord", hostTheme(HostState(settings = mapOf("app.theme" to setting))))
        assertEquals(null, hostTheme(HostState()))
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
