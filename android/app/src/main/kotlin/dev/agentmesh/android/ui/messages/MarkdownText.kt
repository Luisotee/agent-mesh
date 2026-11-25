package dev.agentmesh.android.ui.messages

import androidx.compose.runtime.Composable
import com.mikepenz.markdown.m3.Markdown
import com.mikepenz.markdown.model.rememberMarkdownState

/**
 * Markdown as the message bodies are written. The library parses off the main thread ([rememberMarkdownState]),
 * so a long body never blocks a frame; it is parsed again when the row comes back into view.
 */
@Composable
fun MarkdownText(text: String) {
    val state = rememberMarkdownState(text, retainState = true)
    Markdown(state)
}
