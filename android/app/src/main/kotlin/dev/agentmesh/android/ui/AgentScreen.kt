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
import androidx.compose.material3.TabRow
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.roleLabel
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.toMillis
import dev.agentmesh.android.ui.messages.MessagesTab
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.Run
import java.text.DateFormat
import java.util.Date

/** One agent: its overview, its conversation, and its terminal (read only). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AgentScreen(runtime: HostRuntime, agentId: String, onBack: () -> Unit, initialTab: Tab = Tab.Overview) {
    val container = LocalContainer.current
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val agent = state.agents[agentId]
    val run = agent?.let { state.runs[it.runId] }
    var tab by rememberSaveable { mutableStateOf(initialTab) }
    // Looking at an agent answers its notification.
    LaunchedEffect(agentId) { container.notifier.cancel(runtime.hostId, agentId) }
    // While it is on screen and the app resumed, nothing new is posted for it (see AppContainer.viewing).
    LifecycleResumeEffect(agentId) {
        val me = runtime.hostId to agentId
        container.viewing.value = me
        container.notifier.cancel(runtime.hostId, agentId)
        onPauseOrDispose { container.viewing.compareAndSet(me, null) }
    }
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
        Column(Modifier.padding(pad).fillMaxSize()) {
            Row(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                StateIcon(agent.state, agent.exitCode, agent.stateDetail, statusSentence(agent), size = 24.dp)
                Column(Modifier.padding(start = 12.dp)) {
                    Text(statusSentence(agent), style = MaterialTheme.typography.titleSmall)
                    Text(roleLabel(agent.role), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
            TabRow(selectedTabIndex = tab.ordinal) {
                Tab.entries.forEach { t ->
                    androidx.compose.material3.Tab(selected = tab == t, onClick = { tab = t }, text = { Text(t.name) })
                }
            }
            when (tab) {
                Tab.Overview -> Overview(agent, run, state.clockSkewMs)
                Tab.Messages -> MessagesTab(runtime, agentId, Modifier.fillMaxSize())
                Tab.Terminal -> TerminalTab(runtime, agentId, Modifier.fillMaxSize())
            }
        }
    }
}

@Composable
private fun Overview(agent: Agent, run: Run?, skewMs: Long) {
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
        KeyValue("Run", run?.title?.ifEmpty { run.id } ?: agent.runId)
        KeyValue("Project", repoName(agent.repo))
        KeyValue("Branch", agent.branch, mono = true)
        KeyValue("CLI", agent.kind)
        KeyValue("Model", listOf(agent.model, agent.effort).filter { it.isNotEmpty() }.joinToString(" · "))
        KeyValue("Last activity", if (agent.hasLastActivity()) DateFormat.getDateTimeInstance().format(Date(agent.lastActivity.toMillis() - skewMs)) else "")
        KeyValue("Worktree", agent.worktree, mono = true)
        KeyValue("Conversation", agent.sessionId, mono = true)
    }
}
