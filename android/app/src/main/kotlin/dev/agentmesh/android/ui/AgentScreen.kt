package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.roleLabel
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.store.toMillis
import java.text.DateFormat
import java.util.Date

/** One agent: its role, state and where it works. Messages and the terminal join it in the next parts. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AgentScreen(runtime: HostRuntime, agentId: String, onBack: () -> Unit) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val agent = state.agents[agentId]
    val run = agent?.let { state.runs[it.runId] }
    Scaffold(topBar = {
        TopAppBar(
            title = { Text(agent?.name ?: "Agent") },
            navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } },
        )
    }) { pad ->
        if (agent == null) {
            Text("This agent isn't on the host (any more).", Modifier.padding(pad).padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
            return@Scaffold
        }
        Column(Modifier.padding(pad).fillMaxSize().verticalScroll(rememberScrollState())) {
            Row(Modifier.padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
                StateIcon(agent.state, agent.exitCode, agent.stateDetail, statusSentence(agent), size = 28.dp)
                Column(Modifier.padding(start = 12.dp)) {
                    Text(statusSentence(agent), style = MaterialTheme.typography.titleMedium)
                    Text(roleLabel(agent.role), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
            KeyValue("Run", run?.title?.ifEmpty { run.id } ?: agent.runId)
            KeyValue("Project", repoName(agent.repo))
            KeyValue("Branch", agent.branch, mono = true)
            KeyValue("CLI", agent.kind)
            KeyValue("Model", listOf(agent.model, agent.effort).filter { it.isNotEmpty() }.joinToString(" · "))
            KeyValue("Last activity", if (agent.hasLastActivity()) DateFormat.getDateTimeInstance().format(Date(agent.lastActivity.toMillis() - state.clockSkewMs)) else "")
            KeyValue("Worktree", agent.worktree, mono = true)
            KeyValue("Conversation", agent.sessionId, mono = true)
        }
    }
}
