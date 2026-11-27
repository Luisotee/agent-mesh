package dev.agentmesh.android

import dev.agentmesh.android.derive.hueOf
import dev.agentmesh.android.theme.HUES
import dev.agentmesh.android.theme.Themes
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

class HueTest {
    /** Vectors computed with the desktop's `hueOf` (`desktop/src/hue.ts`, run under node): the same project is the same colour on both apps. */
    @Test fun `a project's hue is the desktop's`() {
        val want = mapOf(
            "/home/u/projects/agent-mesh" to 5, "/home/u/web" to 1, "C:\\src\\api" to 7, "/a" to 6, "/b" to 5, "" to 1,
            "/home/u/billing" to 2, "/home/u/agent-mesh" to 4, "/home/u/shop-api" to 8, "/home/ubuntu/é" to 6,
        )
        for ((path, hue) in want) assertEquals(path, hue, hueOf(path))
    }

    @Test fun `hues are one of the eight and spread over them`() {
        assertEquals(HUES, (0 until 64).map { hueOf("/home/u/projects/repo-$it") }.toSet().size)
        assertTrue((0 until 200).all { hueOf("/r/$it") in 1..HUES })
    }

    @Test fun `every theme has all eight hues, the vendors and the glow, as the desktop's tests ask`() {
        for (f in File("../../shared/themes").listFiles { x -> x.extension == "json" }!!) {
            val spec = Themes.parse(f.readText())
            val t = Themes.resolve(spec)
            for (i in 1..HUES) assertTrue("${spec.name} hue-$i", spec.tokens["hue-$i"]?.matches(Regex("#[0-9a-f]{6}")) == true)
            assertEquals(HUES, t.hues.size)
            assertEquals(HUES, t.hues.toSet().size)
            for (k in listOf("vendor-claude", "vendor-gemini", "vendor-openai", "vendor-copilot", "vendor-opencode", "vendor-deepseek", "glow")) assertTrue("${spec.name} $k", k in spec.tokens)
        }
    }

    @Test fun `Dracula's hues and brand colours are the desktop's`() {
        val t = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
        assertEquals(androidx.compose.ui.graphics.Color(0xFFFF79C6), t.hues[0])
        assertEquals(androidx.compose.ui.graphics.Color(0xFFD97757), t.vendors.claude)
        assertEquals(androidx.compose.ui.graphics.Color(0xFFFFB86C), t.glow)
    }
}
