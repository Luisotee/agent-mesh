package dev.agentmesh.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.Waiting
import dev.agentmesh.android.derive.WaitingKind
import dev.agentmesh.android.ui.messages.AnswerForm
import dev.agentmesh.android.ui.messages.MarkdownText
import dev.agentmesh.android.ui.messages.isLong
import dev.agentmesh.android.ui.messages.preview

/**
 * One thing that waits on the user. A plan or a question opens in place (what it says, and the Approve or the answer),
 * so the user needn't leave the list; a prompt in a terminal opens the agent's terminal.
 */
@Composable
fun WaitingRow(runtime: HostRuntime, w: Waiting, canAct: Boolean, onOpen: () -> Unit) {
    val what = when (w.kind) {
        WaitingKind.Plan -> "sent a plan"
        WaitingKind.Question -> "asked a question"
        WaitingKind.Input -> "needs you in its terminal"
    }
    val answerable = w.kind != WaitingKind.Input && w.message != null
    var open by rememberSaveable(w.message?.id ?: w.agent.id) { mutableStateOf(false) }
    Column(Modifier.fillMaxWidth().testTag("waiting-row")) {
        Row(
            Modifier.fillMaxWidth().clickable(onClick = { if (answerable) open = !open else onOpen() }).padding(horizontal = 16.dp, vertical = 10.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            StateIcon(w.agent.state, w.agent.exitCode, w.agent.stateDetail, "needs you")
            Column(Modifier.padding(start = 12.dp).weight(1f)) {
                Text("${w.agent.name} $what", maxLines = 1, overflow = TextOverflow.Ellipsis)
                w.run?.let { Text(it.title.ifEmpty { it.id }, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis) }
            }
            if (answerable) Text(if (open) "▾" else "▸", color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        if (answerable && open) {
            val m = w.message!!
            Column(Modifier.padding(start = 52.dp, end = 16.dp, bottom = 12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                var all by rememberSaveable(m.id) { mutableStateOf(false) }
                val long = isLong(m.body)
                MarkdownText(if (long && !all) preview(m.body) else m.body, large = long && all)
                if (long) TextButton(onClick = { all = !all }) { Text(if (all) "Show less" else "Show all") }
                if (canAct) AnswerForm(runtime, m) else Text("This device is read-only: it can read, not answer.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                TextButton(onClick = onOpen) { Text("Open the conversation") }
            }
        }
    }
}
