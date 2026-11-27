package dev.agentmesh.android.ui.messages

import dev.agentmesh.android.ui.motionItem
import dev.agentmesh.android.ui.SkeletonRows
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material3.AssistChip
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.messageLabel
import dev.agentmesh.android.derive.messagesOf
import dev.agentmesh.android.derive.waitingFor
import dev.agentmesh.android.ui.collectAsStateWithLifecycleCompat
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import kotlinx.coroutines.launch

private val COMPOSE_TYPES = listOf(MessageType.MESSAGE_TYPE_TASK, MessageType.MESSAGE_TYPE_ANSWER, MessageType.MESSAGE_TYPE_INFO)

/**
 * An agent's conversation with the rest of its run (what it sent and was sent, oldest first), with a plan or a
 * question that waits on the user answerable in place, and a composer. The list is lazy and keyed by message id,
 * so a transcript of thousands costs only the rows on screen; it follows new messages only while the user is at
 * the bottom, so reading older ones is never interrupted.
 */
@Composable
fun MessagesTab(runtime: HostRuntime, agentId: String, modifier: Modifier = Modifier) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val list = remember(state.messages, agentId) { messagesOf(state.messages, agentId) }
    val pending = remember(state.waiting, state.runs, state.agents, state.messages, agentId) {
        waitingFor(state).filter { it.agent.id == agentId }.mapNotNull { it.message?.id }.toSet()
    }
    val listState = rememberLazyListState()
    // At the bottom when the last row is (even partly) in view; then a new message keeps it there.
    // (`list` changes, and a derivedStateOf lambda keeps the value it first saw: read the current one.)
    val current by rememberUpdatedState(list)
    val atBottom by remember { derivedStateOf { val last = listState.layoutInfo.visibleItemsInfo.lastOrNull(); last == null || last.index >= current.lastIndex } }
    var seen by remember { mutableStateOf(0) }
    LaunchedEffect(list.size) {
        if (list.isNotEmpty() && (seen == 0 || atBottom)) listState.scrollToItem(list.lastIndex)
        seen = list.size
    }
    Column(modifier.fillMaxSize().imePadding()) {
        val runId = state.agents[agentId]?.runId
        val failed = runId != null && runId in state.messagesFailed
        // Empty means "none yet" only once the run's history has loaded; before that, and after a load that gave up, it is unknown.
        val known = state.synced && (runId == null || runId in state.messagesLoaded || state.runs[runId]?.status != RunStatus.RUN_STATUS_OPEN)
        if (list.isEmpty()) {
            Column(Modifier.weight(1f).padding(16.dp)) {
                if (!failed && !known) SkeletonRows("Loading the messages", Modifier.testTag("messages-loading"))
                else Text(
                    if (failed) "Couldn't load the messages." else "No messages yet.",
                    color = if (failed) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                )
                if (failed) TextButton(onClick = runtime::reloadMessages, Modifier.testTag("reload-messages")) { Text("Try again") }
            }
        } else {
            LazyColumn(Modifier.weight(1f).fillMaxWidth().testTag("messages"), state = listState, verticalArrangement = Arrangement.spacedBy(8.dp), contentPadding = androidx.compose.foundation.layout.PaddingValues(12.dp)) {
                items(list, key = { it.id }, contentType = { it.type }) { m ->
                    Box(motionItem()) { MessageCard(runtime, m, agentId, open = m.id in pending, skewMs = state.clockSkewMs, canAnswer = !dev.agentmesh.android.ui.chat.isReadOnlyDevice(state)) }
                }
                if (failed) item(key = "load-failed") {
                    TextButton(onClick = runtime::reloadMessages, Modifier.testTag("reload-messages")) { Text("Older messages didn't load. Try again") }
                }
            }
        }
        Composer(runtime, agentId)
    }
}

@Composable
private fun Composer(runtime: HostRuntime, agentId: String) {
    var text by remember(agentId) { mutableStateOf("") }
    var type by remember(agentId) { mutableStateOf(MessageType.MESSAGE_TYPE_TASK) }
    var menu by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()
    fun send() {
        if (busy || text.isBlank()) return
        busy = true; error = null
        scope.launch {
            try { runtime.send(agentId, type, text.trim()); text = "" } catch (e: Exception) { error = e.message ?: e.javaClass.simpleName }
            busy = false
        }
    }
    Column(Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 8.dp)) {
        error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column {
                AssistChip(onClick = { menu = true }, label = { Text(messageLabel(type)) })
                DropdownMenu(menu, { menu = false }) {
                    COMPOSE_TYPES.forEach { t -> DropdownMenuItem(text = { Text(messageLabel(t)) }, onClick = { type = t; menu = false }) }
                }
            }
            OutlinedTextField(text, { text = it }, Modifier.weight(1f).padding(horizontal = 8.dp), placeholder = { Text("Message the agent") }, maxLines = 5)
            IconButton(onClick = ::send, enabled = !busy && text.isNotBlank()) { Icon(Icons.AutoMirrored.Filled.Send, "Send") }
        }
    }
}
