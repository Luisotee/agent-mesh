package dev.agentmesh.android

import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithContentDescription
import dev.agentmesh.android.derive.Vendor
import dev.agentmesh.android.derive.vendorOf
import dev.agentmesh.android.derive.vendorOfKind
import dev.agentmesh.android.derive.vendorOfModel
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.VENDOR_MARKS
import dev.agentmesh.android.ui.VendorIcon
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** The desktop's `vendor.test.ts`, case for case, and a check that the marks are the desktop's own strings. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36])
class VendorTest {
    @get:Rule val compose = createComposeRule()

    @Test fun `a CLI is known by its kind`() {
        assertEquals(
            listOf(Vendor.Claude, Vendor.Gemini, Vendor.OpenCode, Vendor.Copilot, Vendor.OpenAI, Vendor.Claude, Vendor.Other),
            listOf("claude", "agy", "opencode", "copilot", "codex", "Claude ", "mystery").map(::vendorOfKind),
        )
    }

    @Test fun `a model is known by its name or family`() {
        val cases = mapOf(
            "claude-sonnet-5-5" to Vendor.Claude, "opus" to Vendor.Claude, "haiku" to Vendor.Claude, "anthropic/claude-opus-5-5" to Vendor.Claude,
            "gemini-4-neon-high" to Vendor.Gemini, "neon" to Vendor.Gemini, "flash" to Vendor.Gemini, "gemini-3-pro" to Vendor.Gemini,
            "gpt-5" to Vendor.OpenAI, "gpt-5-pro" to Vendor.OpenAI, "o3-pro" to Vendor.OpenAI, "openai/gpt-5-pro" to Vendor.OpenAI, "o3-mini" to Vendor.OpenAI, "openai/gpt-5" to Vendor.OpenAI, "codex" to Vendor.OpenAI,
            "deepseek-v4" to Vendor.DeepSeek, "deepseek-v4-flash" to Vendor.DeepSeek, "deepseek-v4-pro" to Vendor.DeepSeek, "" to Vendor.Other, "llama-4" to Vendor.Other,
        )
        for ((m, v) in cases) assertEquals(m, v, vendorOfModel(m))
    }

    @Test fun `a CLI that runs other vendors' models shows the model's vendor, the others their own`() {
        assertEquals(Vendor.DeepSeek, vendorOf("opencode", "deepseek-v4"))
        assertEquals(Vendor.Claude, vendorOf("copilot", "claude-sonnet-5-5"))
        assertEquals(Vendor.OpenCode, vendorOf("opencode", ""))
        assertEquals(Vendor.Claude, vendorOf("claude", "gpt-5"))
        assertEquals(Vendor.Gemini, vendorOf("agy", "sonnet"))
        assertEquals(Vendor.Gemini, vendorOf("unknown-cli", "gemini-4"))
    }

    /** The marks are the desktop's strings, so an icon is the same on both apps; a new or changed mark there fails here until `VendorMarks.kt` follows. */
    @Test fun `the marks are the desktop's`() {
        val ts = File("../../desktop/src/vendor-marks.ts").readText()
        val theirs = Regex("""(?m)^\s*(\w+): '([^']+)',?$""").findAll(ts).associate { it.groupValues[1] to it.groupValues[2] }
        assertEquals(theirs, VENDOR_MARKS)
    }

    @Test fun `an icon names its vendor, a mark for most and a letter for the rest`() {
        val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
        compose.setContent {
            AgentMeshTheme(theme) {
                androidx.compose.foundation.layout.Column {
                    VendorIcon("claude")
                    VendorIcon("codex")
                    VendorIcon("mystery")
                }
            }
        }
        compose.onNodeWithContentDescription("Anthropic Claude").assertIsDisplayed()
        compose.onNodeWithContentDescription("OpenAI").assertIsDisplayed()
        compose.onNodeWithContentDescription("mystery").assertIsDisplayed()
    }
}
