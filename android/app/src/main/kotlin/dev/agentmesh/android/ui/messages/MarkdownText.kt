package dev.agentmesh.android.ui.messages

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import com.mikepenz.markdown.annotator.annotatorSettings
import com.mikepenz.markdown.compose.components.markdownComponents
import com.mikepenz.markdown.compose.elements.MarkdownHighlightedCodeBlock
import com.mikepenz.markdown.compose.elements.MarkdownHighlightedCodeFence
import com.mikepenz.markdown.compose.elements.MarkdownTableBasicText
import com.mikepenz.markdown.m3.Markdown
import com.mikepenz.markdown.m3.markdownTypography
import com.mikepenz.markdown.model.markdownPadding
import dev.agentmesh.android.ui.LocalMeshTheme
import dev.agentmesh.android.ui.MonoSmall
import dev.agentmesh.android.ui.Skeleton
import dev.snipme.highlights.Highlights
import dev.snipme.highlights.model.SyntaxThemes
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.intellij.markdown.ast.ASTNode
import org.intellij.markdown.ast.getTextInNode
import org.intellij.markdown.flavours.gfm.GFMElementTypes
import org.intellij.markdown.flavours.gfm.GFMTokenTypes

/** Text with none of Markdown's marks is shown as text: most info and answer messages are, and plain text costs nothing. */
private val MARKS = Regex("""[#*_`>|\[\]~]|^\s*([-+]|\d+\.)\s|^\s*---""", RegexOption.MULTILINE)

fun hasMarkdown(text: String): Boolean = MARKS.containsMatchIn(text)

/** A body of at most this many characters is parsed in place when its parse isn't held yet (about a millisecond on a phone); a longer one waits for the background parse behind a placeholder. */
const val SYNC_PARSE_MAX = 800

/**
 * Markdown as the message bodies are written. The parse comes from [MarkdownCache], so a row has its final height at
 * once and the list never shifts under the user's thumb; a body whose parse isn't held and is too long to parse on the
 * main thread shows a placeholder of about its height until the background parse lands (once: it is held after that).
 */
@Composable
fun MarkdownText(text: String) {
    if (!hasMarkdown(text)) { Text(text, style = MaterialTheme.typography.bodyLarge); return }
    val held = remember(text) { MarkdownCache.get(text) ?: if (text.length <= SYNC_PARSE_MAX) MarkdownCache.parse(text) else null }
    var parsed by remember(text) { mutableStateOf(held) }
    var failed by remember(text) { mutableStateOf(held == null && text.length <= SYNC_PARSE_MAX) }
    if (parsed == null && !failed) LaunchedEffect(text) {
        val s = withContext(Dispatchers.Default) { MarkdownCache.parse(text) }
        if (s != null) parsed = s else failed = true
    }
    val state = parsed
    when {
        state != null -> Markdown(state, typography = messageTypography(), padding = messagePadding(), components = messageComponents(), modifier = Modifier.fillMaxWidth())
        failed -> Text(text, style = MaterialTheme.typography.bodyLarge)
        else -> Pending(text)
    }
}

/** What stands for a body that is being parsed: lines of about the height it will have, so the row doesn't jump much when it lands. */
@Composable
private fun Pending(text: String) {
    val lines = remember(text) { text.lineSequence().sumOf { ((it.length + 39) / 40).coerceAtLeast(1) }.coerceIn(2, 40) }
    Column(Modifier.fillMaxWidth().clearAndSetSemantics { contentDescription = "Loading the message" }) {
        repeat(lines) { i -> Box(Modifier.fillMaxWidth().height(24.dp), contentAlignment = Alignment.CenterStart) { Skeleton(Modifier.fillMaxWidth(if (i == lines - 1) 0.5f else 0.95f), height = 12.dp) } }
    }
}

/**
 * The message type: the library's default puts `#` at the display size, which on a phone is a whole screen. Prose is the
 * chat's 16, headings step down from 20 to 16 and stay in the interface's weights, code is the bundled mono.
 */
@Composable
private fun messageTypography(): com.mikepenz.markdown.model.MarkdownTypography {
    val t = MaterialTheme.typography
    return markdownTypography(
            h1 = t.titleLarge, h2 = t.titleMedium.copy(fontWeight = FontWeight.SemiBold),
            h3 = t.titleMedium, h4 = t.titleSmall, h5 = t.titleSmall, h6 = t.labelLarge,
            text = t.bodyLarge, paragraph = t.bodyLarge,
            code = MonoSmall, inlineCode = MonoSmall,
            ordered = t.bodyLarge, bullet = t.bodyLarge, list = t.bodyLarge,
            table = t.bodyMedium,
        )
}

/**
 * The gaps between blocks. The library's defaults are a page's: a list item has 4 dp above and below it (8 dp between two
 * items, on a 24 dp line), a code block 8 dp round it. On a phone that reads as blank space, so each is about half.
 * `block` is the gap above and below a block: two neighbours are 2 x 3 dp apart.
 */
@Composable
private fun messagePadding() = markdownPadding(
    block = 3.dp, list = 2.dp, listItemTop = 1.dp, listItemBottom = 1.dp, listIndent = 6.dp,
    codeBlock = PaddingValues(horizontal = 10.dp, vertical = 4.dp),
)

/**
 * Code blocks with syntax colours and a header (the language, a copy button), in a palette that follows the theme's dark
 * or light. Each block has a builder of its own: `Highlights.Builder` is mutable (the library sets the code and the
 * language on it) and the blocks of a message are highlighted at the same time on background threads, so a shared one
 * mixes their code and leaks one block's language into the next. `key(dark)` restarts a block's highlighting when the
 * theme flips, which the library does not (it keys on the code alone). Tables are the app's own ([MeshTable]).
 */
@Composable
private fun messageComponents(): com.mikepenz.markdown.compose.components.MarkdownComponents {
    val dark = LocalMeshTheme.current.dark
    return markdownComponents(
            codeBlock = {
                key(dark) {
                    val builder = remember { Highlights.Builder().theme(SyntaxThemes.atom(darkMode = dark)) }
                    MarkdownHighlightedCodeBlock(content = it.content, node = it.node, highlightsBuilder = builder, showHeader = true)
                }
            },
            codeFence = {
                key(dark) {
                    val builder = remember { Highlights.Builder().theme(SyntaxThemes.atom(darkMode = dark)) }
                    MarkdownHighlightedCodeFence(content = it.content, node = it.node, highlightsBuilder = builder, showHeader = true)
                }
            },
            table = { MeshTable(it.content, it.node, it.typography.table) },
        )
}

/**
 * A table that can be read on a phone: the columns are as wide as their text up to [MAX_COLUMN], the cells wrap beyond that,
 * and a table wider than the screen scrolls sideways. (The library's own cuts every cell to one line with an ellipsis and
 * gives each column an equal share of the width.)
 */
@Composable
private fun MeshTable(content: String, node: ASTNode, style: TextStyle) {
    val header = remember(node) { node.children.firstOrNull { it.type == GFMElementTypes.HEADER } }
    val rows = remember(node) { node.children.filter { it.type == GFMElementTypes.ROW } }
    fun cellsOf(n: ASTNode) = n.children.filter { it.type == GFMTokenTypes.CELL }
    val head = remember(header) { header?.let(::cellsOf).orEmpty() }
    val body = remember(rows) { rows.map(::cellsOf) }
    val widths = remember(content, node) {
        val all = listOf(head) + body
        List(all.maxOf { it.size }) { c -> columnWidth(all.maxOf { r -> r.getOrNull(c)?.getTextInNode(content)?.trim()?.length ?: 0 }) }
    }
    if (widths.isEmpty()) return
    val annotator = annotatorSettings()
    val colors = MaterialTheme.colorScheme
    val shape = RoundedCornerShape(8.dp)
    Column(Modifier.padding(vertical = 4.dp).clip(shape).border(1.dp, colors.outlineVariant, shape).horizontalScroll(rememberScrollState()).testTag("md-table")) {
        @Composable fun Line(cells: List<ASTNode>, bold: Boolean, background: Color) {
            Row(Modifier.background(background).height(IntrinsicSize.Min)) {
                widths.forEachIndexed { c, w ->
                    Box(Modifier.width(w).padding(horizontal = 10.dp, vertical = 6.dp)) {
                        cells.getOrNull(c)?.let { MarkdownTableBasicText(content, it, if (bold) style.copy(fontWeight = FontWeight.SemiBold) else style, Int.MAX_VALUE, TextOverflow.Clip, annotator) }
                    }
                }
            }
        }
        if (head.isNotEmpty()) { Line(head, true, colors.surfaceContainerHigh); HorizontalDivider(color = colors.outlineVariant) }
        body.forEachIndexed { i, r ->
            Line(r, false, if (i % 2 == 1) colors.surfaceContainerLow else Color.Transparent)
            if (i < body.size - 1) HorizontalDivider(color = colors.outlineVariant.copy(alpha = 0.5f))
        }
    }
}

private val MAX_COLUMN = 240.dp

/** A column as wide as [chars] characters of body text (about 7.5 dp each) and its padding, within 64 dp and [MAX_COLUMN]. */
internal fun columnWidth(chars: Int): Dp = (chars * 7.5f + 20f).dp.coerceIn(64.dp, MAX_COLUMN)
