package dev.agentmesh.android.ui.chat

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.input.InputTransformation
import androidx.compose.foundation.text.input.TextFieldBuffer
import androidx.compose.foundation.text.input.TextFieldLineLimits
import androidx.compose.foundation.text.input.TextFieldState
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Shape
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.isCtrlPressed
import androidx.compose.ui.input.key.isMetaPressed
import androidx.compose.ui.input.key.isShiftPressed
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.TextRange
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.chat.Edit
import dev.agentmesh.android.chat.Field
import dev.agentmesh.android.chat.MarkdownEdit

/**
 * The one multi-line field of the app (the chat's composer, a message, an answer, a change request): Markdown-aware.
 * Enter on a list item continues the list (an empty item ends it), and while focused a row above the keyboard wraps the
 * selection (bold, italic, code), makes lines a list or a checklist, or a link. Enter otherwise adds a line: on a phone
 * Send is the button. The field grows with its text up to 40% of the screen, then scrolls. The rules are
 * [MarkdownEdit]; this only applies them. [tag] is the text field's own test tag; [trailing] stands beside the box (Send).
 */
@Composable
fun MarkdownField(
    state: TextFieldState,
    modifier: Modifier = Modifier,
    tag: String? = null,
    enabled: Boolean = true,
    placeholder: String = "",
    accent: Color = MaterialTheme.colorScheme.primary,
    minLines: Int = 1,
    shape: Shape = RoundedCornerShape(20.dp),
    formatting: Boolean = true,
    trailing: (@Composable () -> Unit)? = null,
) {
    var focused by remember { mutableStateOf(false) }
    val maxHeight = (LocalConfiguration.current.screenHeightDp * 0.4f).dp
    val s = MaterialTheme.colorScheme
    Column(modifier, verticalArrangement = Arrangement.spacedBy(6.dp)) {
        Row(verticalAlignment = Alignment.Bottom) {
            Box(Modifier.weight(1f).clip(shape).border(BorderStroke(if (focused) 2.dp else 1.dp, if (focused) accent else s.outlineVariant), shape)) {
                BasicTextField(
                    state,
                    Modifier.fillMaxWidth().heightIn(max = maxHeight).padding(horizontal = 16.dp, vertical = 10.dp)
                        .onFocusChanged { focused = it.isFocused }
                        .onPreviewKeyEvent { hardwareKey(state, it) }
                        .then(if (tag != null) Modifier.testTag(tag) else Modifier),
                    enabled = enabled,
                    textStyle = MaterialTheme.typography.bodyLarge.copy(color = if (enabled) s.onSurface else s.onSurfaceVariant),
                    cursorBrush = SolidColor(accent),
                    inputTransformation = ContinueList,
                    lineLimits = TextFieldLineLimits.MultiLine(minHeightInLines = minLines),
                    decorator = { inner ->
                        Box {
                            if (state.text.isEmpty() && placeholder.isNotEmpty()) Text(placeholder, style = MaterialTheme.typography.bodyLarge, color = s.onSurfaceVariant)
                            inner()
                        }
                    },
                )
            }
            trailing?.invoke()
        }
        if (formatting && enabled && focused) FormatRow(state, accent)
    }
}

/** Enter on a list item: the new line is the next item (the key arrives as a typed newline, from the soft keyboard or a hardware one). */
private val ContinueList = InputTransformation {
    val before = originalText
    val sel = originalSelection
    val after = asCharSequence()
    if (!sel.collapsed || after.length != before.length + 1) return@InputTransformation
    val at = sel.start
    // Only a lone newline typed at the cursor: not a paste, not a correction.
    if (after[at] != '\n' || !after.startsWith(before.subSequence(0, at)) || after.subSequence(at + 1, after.length).toString() != before.subSequence(at, before.length).toString()) return@InputTransformation
    MarkdownEdit.enter(Field(before.toString(), at, at))?.let { put(MarkdownEdit.apply(Field(before.toString(), at, at), it)) }
}

private fun TextFieldBuffer.put(f: Field) {
    replace(0, length, f.value)
    selection = TextRange(f.start, f.end)
}

/** Applies an edit to the field as one change, which the field's own undo takes back. */
private fun TextFieldState.apply(edit: (Field) -> Edit?) {
    val f = Field(text.toString(), selection.min, selection.max)
    val e = edit(f) ?: return
    val next = MarkdownEdit.apply(f, e)
    edit { put(next) }
}

/** A hardware keyboard: Ctrl or Cmd with B, I, E, K as on the desktop, and Tab or Shift+Tab on a list item or a selection. */
private fun hardwareKey(state: TextFieldState, e: androidx.compose.ui.input.key.KeyEvent): Boolean {
    if (e.type != KeyEventType.KeyDown) return false
    val mod = e.isCtrlPressed || e.isMetaPressed
    val edit: ((Field) -> Edit?)? = when {
        mod && !e.isShiftPressed && e.key == Key.B -> { f -> MarkdownEdit.bold(f) }
        mod && !e.isShiftPressed && e.key == Key.I -> { f -> MarkdownEdit.italic(f) }
        mod && !e.isShiftPressed && e.key == Key.E -> { f -> MarkdownEdit.code(f) }
        mod && !e.isShiftPressed && e.key == Key.K -> { f -> MarkdownEdit.link(f) }
        !mod && e.key == Key.Tab -> { f -> MarkdownEdit.indent(f, out = e.isShiftPressed) }
        else -> null
    }
    if (edit == null) return false
    // A key with nothing to do (Tab on plain text) is left to move the focus on.
    val f = Field(state.text.toString(), state.selection.min, state.selection.max)
    if (edit(f) == null) return false
    state.apply(edit)
    return true
}

/** The buttons above the keyboard: they act on the selection of the focused field (a tap does not take the focus from it). */
@Composable
private fun FormatRow(state: TextFieldState, accent: Color) {
    Row(Modifier.fillMaxWidth().testTag("format-row"), horizontalArrangement = Arrangement.spacedBy(4.dp), verticalAlignment = Alignment.CenterVertically) {
        FormatButton("Bold", "B", "format-bold", accent, weight = FontWeight.Bold) { state.apply { MarkdownEdit.bold(it) } }
        FormatButton("Italic", "I", "format-italic", accent, style = FontStyle.Italic) { state.apply { MarkdownEdit.italic(it) } }
        FormatButton("Code", "<>", "format-code", accent, mono = true) { state.apply { MarkdownEdit.code(it) } }
        FormatButton("List", "•", "format-list", accent) { state.apply { MarkdownEdit.list(it, task = false) } }
        FormatButton("Checklist", "☐", "format-checklist", accent) { state.apply { MarkdownEdit.list(it, task = true) } }
        FormatButton("Link", "↗", "format-link", accent) { state.apply { MarkdownEdit.link(it) } }
    }
}

@Composable
private fun FormatButton(
    label: String, glyph: String, tag: String, accent: Color, weight: FontWeight? = null, style: FontStyle? = null, mono: Boolean = false, onClick: () -> Unit,
) {
    Box(
        Modifier.size(40.dp, 36.dp).clip(RoundedCornerShape(10.dp)).clickable(onClick = onClick).semantics { contentDescription = label; role = Role.Button }.testTag(tag),
        contentAlignment = Alignment.Center,
    ) {
        Text(glyph, color = accent, style = MaterialTheme.typography.titleMedium, fontWeight = weight, fontStyle = style, fontFamily = if (mono) FontFamily.Monospace else null)
    }
}
