package dev.agentmesh.android.ui

import android.widget.Toast
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.chat.say
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch

/** What the user can do to an agent or a run that ends or renames it. Each is confirmed in a dialog before it is sent. */
sealed interface AgentAction {
    data class StopAgent(val agentId: String, val name: String) : AgentAction
    data class RetireAgent(val agentId: String, val name: String) : AgentAction
    data class StopRun(val runId: String, val title: String) : AgentAction
    data class DeleteRun(val runId: String, val title: String) : AgentAction
    data class RenameRun(val runId: String, val title: String) : AgentAction
    /** A project shell: ending it hangs up its programs, as closing a terminal window does. */
    data class EndShell(val shellId: String, val project: String) : AgentAction
}

/**
 * The dialog for [action]: says what it will do, and sends it only when the user confirms. The host's own words come back
 * on a refusal. [onDone] runs after the call succeeded (true) or the user gave up (false).
 */
@Composable
fun AgentActionDialog(runtime: HostRuntime, action: AgentAction, onDone: (succeeded: Boolean) -> Unit) {
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf("") }
    var title by remember(action) { mutableStateOf((action as? AgentAction.RenameRun)?.title.orEmpty()) }
    val scope = rememberCoroutineScope()
    val context = LocalContext.current
    val prefs = LocalContainer.current.prefs
    val (head, body, verb) = when (action) {
        is AgentAction.StopAgent -> Triple("Stop ${action.name}?", "Its process ends now. Its worktree and branch stay.", "Stop")
        is AgentAction.RetireAgent -> Triple("Retire ${action.name}?", "It stops on purpose and keeps its branch. A message to it brings it back.", "Retire")
        is AgentAction.StopRun -> Triple("Stop ${action.title}?", "Every agent in the run is retired and the finished worktrees go. A worktree with changes stays.", "Stop the run")
        is AgentAction.DeleteRun -> Triple("Delete ${action.title}?", "Its chats, messages, plans and attachments are removed from the host for good, and so are its lines in the cost report. Work in git stays: a worktree with changes, or a branch with commits no remote has, is kept on disk.", "Delete the run")
        is AgentAction.RenameRun -> Triple("Rename the run", "", "Rename")
        is AgentAction.EndShell -> Triple("End the terminal in ${action.project}?", "What runs in it gets a hangup, as when a terminal window is closed. Leaving the screen instead keeps it running.", "End it")
    }
    val connected = runtime.state.collectAsStateWithLifecycleCompat().value.conn is dev.agentmesh.android.store.ConnState.Ready
    fun confirm() {
        if (busy) return
        busy = true
        error = ""
        scope.launch {
            try {
                val api = runtime.connectedApi()
                var kept = emptyList<String>()
                when (action) {
                    is AgentAction.StopAgent -> api.stopAgent(action.agentId)
                    is AgentAction.RetireAgent -> api.retireAgent(action.agentId)
                    is AgentAction.StopRun -> api.stopRun(action.runId)
                    is AgentAction.DeleteRun -> kept = api.deleteRun(action.runId)
                    is AgentAction.RenameRun -> api.renameRun(action.runId, title.trim())
                    is AgentAction.EndShell -> { runtime.closeShell(action.shellId); prefs.forgetPhoneShell(runtime.hostId, action.shellId) }
                }
                if (kept.isNotEmpty()) Toast.makeText(context, "Deleted. Kept on disk, with its work: ${kept.joinToString("; ")}", Toast.LENGTH_LONG).show()
                onDone(true)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                error = say(e)
            } finally {
                busy = false
            }
        }
    }
    val rename = action as? AgentAction.RenameRun
    AlertDialog(
        onDismissRequest = { if (!busy) onDone(false) },
        title = { Text(head) },
        text = {
            Column {
                if (body.isNotEmpty()) Text(body)
                if (rename != null) OutlinedTextField(title, { title = it }, Modifier.fillMaxWidth().testTag("action-title"), label = { Text("Title") }, singleLine = true)
                if (error.isNotEmpty()) Text(error, Modifier.testTag("action-error"), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                if (!connected) Text("Not connected to the host: reconnect first.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        },
        confirmButton = {
            TextButton(onClick = ::confirm, enabled = !busy && connected && (rename == null || (title.isNotBlank() && title.trim() != rename.title)), modifier = Modifier.testTag("action-confirm")) { Text(verb) }
        },
        dismissButton = { TextButton(onClick = { if (!busy) onDone(false) }, modifier = Modifier.testTag("action-cancel")) { Text("Cancel") } },
    )
}
