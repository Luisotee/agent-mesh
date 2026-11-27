package dev.agentmesh.android.ui.messages

import dev.agentmesh.android.ui.motionSize
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.foundation.shape.RoundedCornerShape
import dev.agentmesh.android.ui.chat.MarkdownField
import androidx.compose.foundation.text.input.clearText
import androidx.compose.foundation.text.input.rememberTextFieldState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.messageLabel
import dev.agentmesh.android.store.toMillis
import dev.agentmesh.android.ui.stateColors
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.MessageType
import kotlinx.coroutines.launch
import java.text.DateFormat
import java.util.Date

/** A body longer than this is shown cut to [PREVIEW_LINES] lines until the user asks for all of it. */
const val PREVIEW_CHARS = 700
const val PREVIEW_LINES = 12

/** The part of [body] a collapsed card shows: its first lines, cut at a line end. */
fun preview(body: String): String {
    val lines = body.lines()
    val cut = lines.take(PREVIEW_LINES).joinToString("\n")
    return if (lines.size > PREVIEW_LINES || body.length > PREVIEW_CHARS) cut.take(PREVIEW_CHARS).trimEnd() + "\n…" else body
}

fun isLong(body: String) = body.length > PREVIEW_CHARS || body.count { it == '\n' } >= PREVIEW_LINES

@Composable
fun MessageCard(runtime: HostRuntime, m: Message, viewing: String, open: Boolean, skewMs: Long, canAnswer: Boolean = true) {
    val colors = stateColors
    val accent = when (m.type) {
        MessageType.MESSAGE_TYPE_PLAN -> MaterialTheme.colorScheme.primary
        MessageType.MESSAGE_TYPE_QUESTION -> colors.needs
        MessageType.MESSAGE_TYPE_DONE -> colors.done
        MessageType.MESSAGE_TYPE_FAILED -> colors.failed
        else -> MaterialTheme.colorScheme.onSurfaceVariant
    }
    val mine = m.from.id == viewing
    var expanded by rememberSaveable(m.id) { mutableStateOf(false) }
    // The size animates only for the user's own Show all / Show less: a card that scrolls into view is laid out at its height at once.
    var toggled by remember { mutableStateOf(false) }
    val long = isLong(m.body)
    Card(
        Modifier.fillMaxWidth().then(if (toggled) Modifier.motionSize() else Modifier).testTag("message"),
        colors = CardDefaults.cardColors(containerColor = if (open) MaterialTheme.colorScheme.surfaceContainerHigh else MaterialTheme.colorScheme.surfaceContainer),
    ) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(messageLabel(m.type), color = accent, style = MaterialTheme.typography.labelLarge)
                Text(if (mine) "to ${m.toName.ifEmpty { m.toId }}" else "from ${m.from.name.ifEmpty { m.from.id }}", Modifier.weight(1f), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                if (open) Text("waiting for you", style = MaterialTheme.typography.labelSmall, color = colors.needs)
                Text(stamp(m, skewMs), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            MarkdownText(if (long && !expanded) preview(m.body) else m.body)
            if (long) TextButton(onClick = { toggled = true; expanded = !expanded }) { Text(if (expanded) "Show less" else "Show all") }
            if (open && canAnswer) AnswerForm(runtime, m)
        }
    }
}

private fun stamp(m: Message, skewMs: Long): String =
    if (!m.hasCreatedAt()) "" else DateFormat.getDateTimeInstance(DateFormat.SHORT, DateFormat.SHORT).format(Date(m.createdAt.toMillis() - skewMs))

/** The user's reply to a plan (approve it, or ask for changes) or a question (answer it): all go to the author. */
@Composable
fun AnswerForm(runtime: HostRuntime, m: Message) {
    val author = m.from.id
    val isPlan = m.type == MessageType.MESSAGE_TYPE_PLAN
    val box = rememberTextFieldState()
    val text = box.text.toString()
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()
    fun run(block: suspend () -> Unit) {
        if (busy) return
        busy = true; error = null
        scope.launch {
            try { block(); box.clearText() } catch (e: kotlinx.coroutines.CancellationException) { throw e } catch (e: Exception) { error = e.message ?: e.javaClass.simpleName }
            busy = false
        }
    }
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        MarkdownField(box, Modifier.fillMaxWidth(), tag = "answer", placeholder = if (isPlan) "What should change?" else "Your answer", minLines = 2, shape = RoundedCornerShape(12.dp))
        if (isPlan) Text("To approve, leave this empty.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            if (isPlan) {
                // Typed changes are not an approval: they go with Request changes.
                Button(enabled = !busy && text.isBlank(), onClick = { run { runtime.approve(author, m.id) } }) { Text("Approve") }
                OutlinedButton(enabled = !busy && text.isNotBlank(), onClick = { run { runtime.requestChanges(author, text, m.id) } }) { Text("Request changes") }
            } else {
                Button(enabled = !busy && text.isNotBlank(), onClick = { run { runtime.answer(author, text, m.id) } }) { Text("Send answer") }
            }
        }
        error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
    }
}
