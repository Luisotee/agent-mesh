package dev.agentmesh.android.ui.messages

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.chat.say
import dev.agentmesh.android.derive.messageLabel
import dev.agentmesh.gen.agentmesh.v1.MessageType
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch

private val COMPOSE_TYPES = listOf(MessageType.MESSAGE_TYPE_TASK, MessageType.MESSAGE_TYPE_ANSWER, MessageType.MESSAGE_TYPE_INFO)

/**
 * A mesh message to the agent (a task, an answer or some information), as the host's mailbox takes it: not the same as
 * typing into its terminal, which the chat's composer does. A sheet, since it is rarer than chatting.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SendMessageSheet(runtime: HostRuntime, agentId: String, onDismiss: () -> Unit) {
    var text by rememberSaveable(agentId) { mutableStateOf("") }
    var type by rememberSaveable(agentId) { mutableStateOf(MessageType.MESSAGE_TYPE_TASK) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf("") }
    val scope = rememberCoroutineScope()
    fun send() {
        if (busy || text.isBlank()) return
        busy = true
        error = ""
        scope.launch {
            try {
                runtime.send(agentId, type, text.trim())
                text = ""
                onDismiss()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                error = say(e)
            } finally {
                busy = false
            }
        }
    }
    ModalBottomSheet(onDismissRequest = { if (!busy) onDismiss() }) {
        Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp).padding(bottom = 16.dp).navigationBarsPadding().imePadding().testTag("send-message-sheet"), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("Send a message", style = MaterialTheme.typography.titleMedium)
            Text("It goes to the agent's mailbox, as the mesh's own messages do.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                COMPOSE_TYPES.forEach { t -> FilterChip(selected = type == t, onClick = { type = t }, label = { Text(messageLabel(t)) }, modifier = Modifier.testTag("type-${messageLabel(t)}")) }
            }
            OutlinedTextField(text, { text = it }, Modifier.fillMaxWidth().testTag("message-text"), placeholder = { Text("Message the agent") }, minLines = 3, maxLines = 8)
            if (error.isNotEmpty()) Text(error, Modifier.testTag("message-error"), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            Button(onClick = ::send, enabled = !busy && text.isNotBlank(), modifier = Modifier.fillMaxWidth().testTag("message-send")) { Text("Send") }
        }
    }
}
