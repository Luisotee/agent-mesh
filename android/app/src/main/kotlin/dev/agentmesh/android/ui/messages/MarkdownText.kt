package dev.agentmesh.android.ui.messages

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import com.mikepenz.markdown.m3.Markdown
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
        !hasMarkdown(text) -> Text(text, style = MaterialTheme.typography.bodyMedium)
        large -> Markdown(rememberMarkdownState(text, retainState = true))
        else -> Markdown(text)
    }
}
