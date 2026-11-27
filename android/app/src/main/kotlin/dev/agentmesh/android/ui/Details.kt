package dev.agentmesh.android.ui

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.CostView
import dev.agentmesh.android.derive.LeadRow
import dev.agentmesh.android.derive.WorkerRow
import dev.agentmesh.android.derive.costView
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.roleLabel
import dev.agentmesh.android.derive.runsOn
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.Run
import kotlinx.coroutines.CancellationException
import java.text.DateFormat
import java.util.Date

/**
 * What the chat leaves out, in a bottom sheet: the agent's pull requests (with Hold), its tree with the reviewers and askers,
 * the run's cost folded to one line, its branch, worktree and conversation, a mesh message to it, and Stop or Retire. A read-only
 * device sees all of it and none of the actions.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DetailsSheet(
    runtime: HostRuntime, agent: Agent, run: Run?, row: LeadRow?, skewMs: Long, canAct: Boolean,
    onOpen: (Agent) -> Unit, onSend: () -> Unit, onDismiss: () -> Unit,
) {
    val context = LocalContext.current
    var action by remember { mutableStateOf<AgentAction?>(null) }
    val live = agent.state != AgentState.AGENT_STATE_EXITED && agent.state != AgentState.AGENT_STATE_RETIRED
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Column(Modifier.fillMaxWidth().verticalScroll(rememberScrollState()).navigationBarsPadding().padding(bottom = 16.dp).testTag("details")) {
            Row(Modifier.padding(horizontal = 16.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                StateIcon(agent.state, agent.exitCode, agent.stateDetail, statusSentence(agent), size = 22.dp)
                Column(Modifier.weight(1f)) {
                    Text(agent.name, style = MaterialTheme.typography.titleMedium)
                    Text("${roleLabel(agent.role)} · ${statusSentence(agent)}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
            PullRequestsSection(runtime, agent.id, canAct)
            if (row != null) AgentsSection(row, agent, onOpen)
            CostLine(runtime, agent.runId)
            HorizontalDivider(Modifier.padding(vertical = 8.dp))
            KeyValue("Run", run?.title?.ifEmpty { run.id } ?: agent.runId)
            KeyValue("Project", repoName(agent.repo))
            KeyValue("Runs on", runsOn(agent).ifEmpty { agent.kind })
            KeyValue("Branch", agent.branch, mono = true)
            KeyValue("Last activity", if (agent.hasLastActivity()) DateFormat.getDateTimeInstance().format(Date(agent.lastActivity.toMillis() - skewMs)) else "")
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) { KeyValue("Worktree", agent.worktree, mono = true) }
                if (agent.worktree.isNotEmpty()) TextButton(onClick = { copyToClipboard(context, agent.worktree, "path") }, Modifier.padding(end = 8.dp).testTag("copy-worktree")) { Text("Copy") }
            }
            KeyValue("Conversation", agent.sessionId, mono = true)
            if (canAct) {
                HorizontalDivider(Modifier.padding(vertical = 8.dp))
                Column(Modifier.padding(horizontal = 16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedButton(onClick = { onDismiss(); onSend() }, Modifier.fillMaxWidth().testTag("details-send")) { Text("Send a message…") }
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedButton(onClick = { action = AgentAction.StopAgent(agent.id, agent.name) }, enabled = live, modifier = Modifier.weight(1f).testTag("details-stop")) { Text("Stop") }
                        OutlinedButton(onClick = { action = AgentAction.RetireAgent(agent.id, agent.name) }, enabled = live, modifier = Modifier.weight(1f).testTag("details-retire")) { Text("Retire") }
                    }
                }
            }
        }
    }
    action?.let { a -> AgentActionDialog(runtime, a) { ok -> action = null; if (ok) onDismiss() } }
}

/** The run's agents: the lead, its workers (nested as they were started), and each one's reviewers and askers under it. A tap opens one. */
@Composable
private fun AgentsSection(row: LeadRow, current: Agent, onOpen: (Agent) -> Unit) {
    SectionHeader("Agents")
    Column(Modifier.testTag("details-agents")) {
        row.lead?.let { AgentRow(it, 0, current, onOpen) }
        row.helpers.forEach { AgentRow(it, 1, current, onOpen) }
        row.workers.forEach { WorkerRows(it, 1, current, onOpen) }
    }
}

@Composable
private fun WorkerRows(w: WorkerRow, depth: Int, current: Agent, onOpen: (Agent) -> Unit) {
    AgentRow(w.agent, depth, current, onOpen)
    w.helpers.forEach { AgentRow(it, depth + 1, current, onOpen) }
    w.workers.forEach { WorkerRows(it, depth + 1, current, onOpen) }
}

@Composable
private fun AgentRow(a: Agent, depth: Int, current: Agent, onOpen: (Agent) -> Unit) {
    val here = a.id == current.id
    Row(
        Modifier.fillMaxWidth().heightIn(min = 44.dp).clickable(enabled = !here) { onOpen(a) }.padding(start = 16.dp + 16.dp * depth, end = 16.dp).testTag("details-agent-${a.id}"),
        verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp),
    ) {
        StateIcon(a.state, a.exitCode, a.stateDetail, statusSentence(a), size = 16.dp)
        Column(Modifier.weight(1f)) {
            Text(a.name, style = MaterialTheme.typography.bodyMedium)
            Text("${roleLabel(a.role)} · ${statusSentence(a)}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
        }
        VendorIcon(a.kind, a.model, size = 16.dp)
    }
}

/** The run's cost as one line that opens to what the Usage screen says of it; it is read when first opened, not before. */
@Composable
private fun CostLine(runtime: HostRuntime, runId: String) {
    var open by rememberSaveable(runId) { mutableStateOf(false) }
    var cost by remember(runId) { mutableStateOf<CostView?>(null) }
    var failed by remember(runId) { mutableStateOf(false) }
    LaunchedEffect(open, runId) {
        if (!open || cost != null) return@LaunchedEffect
        failed = false
        try { cost = costView(runtime.awaitApi().getCost(runId)) } catch (e: CancellationException) { throw e } catch (e: Exception) { failed = true }
    }
    Row(Modifier.fillMaxWidth().clickable { open = !open }.heightIn(min = 48.dp).padding(horizontal = 16.dp).testTag("details-cost"), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text("Cost of the run", style = MaterialTheme.typography.bodyMedium)
            cost?.let { Text("${it.tokens} · ${it.usd} at API prices", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
        }
        Icon(if (open) Icons.Default.KeyboardArrowUp else Icons.Default.KeyboardArrowDown, if (open) "Fold" else "Open")
    }
    AnimatedVisibility(open) {
        Column(Modifier.padding(horizontal = 16.dp).padding(bottom = 8.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            val c = cost
            when {
                failed -> Text("Couldn't read the cost.", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                c == null -> SkeletonLines("Loading the cost", lines = 2)
                else -> {
                    Text(c.breakdown, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    c.notes.forEach { Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                }
            }
        }
    }
}
