package dev.agentmesh.android.ui.messages

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import com.mikepenz.markdown.compose.components.markdownComponents
import com.mikepenz.markdown.compose.elements.MarkdownHighlightedCodeBlock
import com.mikepenz.markdown.compose.elements.MarkdownHighlightedCodeFence
import com.mikepenz.markdown.m3.Markdown
import dev.snipme.highlights.Highlights
import dev.snipme.highlights.model.SyntaxThemes
import com.mikepenz.markdown.m3.markdownTypography
import com.mikepenz.markdown.model.rememberMarkdownState

/** Text with none of Markdown's marks is shown as text: most info and answer messages are, and plain text costs nothing. */
private val MARKS = Regex("""[#*_`>|\[\]~]|^\s*([-+]|\d+\.)\s|^\s*---""", RegexOption.MULTILINE)

fun hasMarkdown(text: String): Boolean = MARKS.containsMatchIn(text)

/**
 * Markdown as the message bodies are written. A body of ordinary text, and a collapsed preview (under a screenful),
 * compose in one pass, so a row has its final height at once and the list never shifts under the user's thumb. A
 * long body the user expanded is parsed off the main thread, so reading it never blocks a frame.
 */
@Composable
fun MarkdownText(text: String, large: Boolean = false) {
    when {
        !hasMarkdown(text) -> Text(text, style = MaterialTheme.typography.bodyLarge)
        large -> Markdown(rememberMarkdownState(text, retainState = true), typography = messageTypography(), components = messageComponents())
        else -> Markdown(text, typography = messageTypography(), components = messageComponents())
    }
}

/**
 * The message type: the library's default puts `#` at the display size, which on a phone is a whole screen. Prose is the
 * chat's 16, headings step down from 20 to 16 and stay in the interface's weights, code is the bundled mono.
 */
@Composable
private fun messageTypography() = markdownTypography(
    h1 = MaterialTheme.typography.titleLarge, h2 = MaterialTheme.typography.titleMedium.copy(fontWeight = androidx.compose.ui.text.font.FontWeight.SemiBold),
    h3 = MaterialTheme.typography.titleMedium, h4 = MaterialTheme.typography.titleSmall, h5 = MaterialTheme.typography.titleSmall, h6 = MaterialTheme.typography.labelLarge,
    text = MaterialTheme.typography.bodyLarge, paragraph = MaterialTheme.typography.bodyLarge,
    code = dev.agentmesh.android.ui.MonoSmall, inlineCode = dev.agentmesh.android.ui.MonoSmall,
    ordered = MaterialTheme.typography.bodyLarge, bullet = MaterialTheme.typography.bodyLarge, list = MaterialTheme.typography.bodyLarge,
)

/**
 * Code blocks with syntax colours and a header (the language, a copy button), in a palette that follows the theme's dark
 * or light. Each block has a builder of its own: `Highlights.Builder` is mutable (the library sets the code and the
 * language on it) and the blocks of a message are highlighted at the same time on background threads, so a shared one
 * mixes their code and leaks one block's language into the next. `key(dark)` restarts a block's highlighting when the
 * theme flips, which the library does not (it keys on the code alone).
 */
@Composable
private fun messageComponents(): com.mikepenz.markdown.compose.components.MarkdownComponents {
    val dark = dev.agentmesh.android.ui.LocalMeshTheme.current.dark
    return markdownComponents(
        codeBlock = {
            androidx.compose.runtime.key(dark) {
                val builder = androidx.compose.runtime.remember { Highlights.Builder().theme(SyntaxThemes.atom(darkMode = dark)) }
                MarkdownHighlightedCodeBlock(content = it.content, node = it.node, highlightsBuilder = builder, showHeader = true)
            }
        },
        codeFence = {
            androidx.compose.runtime.key(dark) {
                val builder = androidx.compose.runtime.remember { Highlights.Builder().theme(SyntaxThemes.atom(darkMode = dark)) }
                MarkdownHighlightedCodeFence(content = it.content, node = it.node, highlightsBuilder = builder, showHeader = true)
            }
        },
    )
}
