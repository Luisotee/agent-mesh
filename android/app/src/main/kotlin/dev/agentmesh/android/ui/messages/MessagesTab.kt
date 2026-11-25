package dev.agentmesh.android.ui.messages

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
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.messageLabel
import dev.agentmesh.android.derive.messagesOf
import dev.agentmesh.android.derive.waitingOnYou
import dev.agentmesh.android.ui.collectAsStateWithLifecycleCompat
import dev.agentmesh.gen.agentmesh.v1.MessageType
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
    val pending = remember(state.runs, state.agents, state.messages, agentId) {
        waitingOnYou(state.runs, state.agents, state.messages).filter { it.agent.id == agentId }.mapNotNull { it.message?.id }.toSet()
    }
    val listState = rememberLazyListState()
    // At the bottom when the last row is (even partly) in view; then a new message keeps it there.
    val atBottom by remember { derivedStateOf { val last = listState.layoutInfo.visibleItemsInfo.lastOrNull(); last == null || last.index >= list.lastIndex } }
    var seen by remember { mutableStateOf(0) }
    LaunchedEffect(list.size) {
        if (list.isNotEmpty() && (seen == 0 || atBottom)) listState.scrollToItem(list.lastIndex)
        seen = list.size
    }
    Column(modifier.fillMaxSize().imePadding()) {
        if (list.isEmpty()) {
            Text(
                if (state.synced) "No messages yet." else "Loading…", Modifier.weight(1f).padding(16.dp),
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        } else {
            LazyColumn(Modifier.weight(1f).fillMaxWidth(), state = listState, verticalArrangement = Arrangement.spacedBy(8.dp), contentPadding = androidx.compose.foundation.layout.PaddingValues(12.dp)) {
                items(list, key = { it.id }, contentType = { it.type }) { m ->
                    MessageCard(runtime, m, agentId, open = m.id in pending, skewMs = state.clockSkewMs)
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
