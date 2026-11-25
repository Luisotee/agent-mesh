package dev.agentmesh.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.AgentRow
import dev.agentmesh.android.derive.RunRow
import dev.agentmesh.android.derive.Waiting
import dev.agentmesh.android.derive.WaitingKind
import dev.agentmesh.android.derive.buildTrees
import dev.agentmesh.android.derive.flatten
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.roleLabel
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.derive.waitingOnYou
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.RunStatus

/** A host's home: its connection, what waits on the user, and each run as a tree from its lead. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HostScreen(runtime: HostRuntime, onBack: () -> Unit, onAgent: (String) -> Unit, onLog: () -> Unit, onSettings: () -> Unit) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val trees = remember(state.runs, state.agents) { buildTrees(state.runs, state.agents) }
    val waiting = remember(state.runs, state.agents, state.messages) { waitingOnYou(state.runs, state.agents, state.messages) }
    // Closed runs start collapsed and open ones expanded; the ids the user flipped are kept across rotation.
    var flipped by rememberSaveable { mutableStateOf(ArrayList<String>()) }
    val rows = remember(trees, flipped) { flatten(trees) { r -> (r.status == RunStatus.RUN_STATUS_OPEN) != (r.id in flipped) } }
    Scaffold(topBar = {
        TopAppBar(
            title = { Text(runtime.host?.name ?: "Host") },
            navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } },
            actions = { IconButton(onClick = onSettings) { Icon(Icons.Default.Settings, "Host settings") } },
        )
    }) { pad ->
        Column(Modifier.padding(pad).fillMaxSize()) {
            ConnectionBanner(state, onRetry = runtime::nudge, onLog = onLog)
            LazyColumn(Modifier.fillMaxSize()) {
                if (waiting.isNotEmpty()) {
                    item(key = "waiting-header") { SectionHeader("Waiting on you (${waiting.size})") }
                    items(waiting, key = { "wait:${it.kind}:${it.message?.id ?: it.agent.id}" }, contentType = { "waiting" }) { w ->
                        WaitingRow(w) { onAgent(w.agent.id) }
                    }
                    item(key = "waiting-divider") { HorizontalDivider(Modifier.padding(top = 8.dp)) }
                }
                if (rows.isEmpty() && state.synced) {
                    item(key = "empty") {
                        Text("No runs yet. Start one from the desktop app or the host's CLI.", Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                items(rows, key = { it.key }, contentType = { if (it is RunRow) "run" else "agent" }) { row ->
                    when (row) {
                        is RunRow -> RunHeader(row) {
                            val id = row.run.id
                            flipped = ArrayList(if (id in flipped) flipped - id else flipped + id)
                        }
                        is AgentRow -> AgentLine(row) { onAgent(row.agent.id) }
                    }
                }
            }
        }
    }
}

@Composable
private fun WaitingRow(w: Waiting, onClick: () -> Unit) {
    val what = when (w.kind) {
        WaitingKind.Plan -> "sent a plan"
        WaitingKind.Question -> "asked a question"
        WaitingKind.Input -> "needs you in its terminal"
    }
    Row(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 10.dp), verticalAlignment = Alignment.CenterVertically) {
        StateIcon(w.agent.state, w.agent.exitCode, w.agent.stateDetail, "needs you")
        Column(Modifier.padding(start = 12.dp)) {
            Text("${w.agent.name} $what", maxLines = 1, overflow = TextOverflow.Ellipsis)
            w.run?.let { Text(it.title.ifEmpty { it.id }, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis) }
        }
    }
}

@Composable
private fun RunHeader(row: RunRow, onToggle: () -> Unit) {
    val closed = row.run.status == RunStatus.RUN_STATUS_CLOSED
    Row(Modifier.fillMaxWidth().clickable(onClick = onToggle).padding(horizontal = 16.dp, vertical = 12.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(if (row.expanded) "▾" else "▸", Modifier.width(20.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
        Column(Modifier.weight(1f)) {
            Text(row.run.title.ifEmpty { row.run.id }, style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
            Text(
                buildString {
                    append(if (closed) "closed" else "${row.live} live")
                    if (row.needsInput > 0) append(", ${row.needsInput} need you")
                },
                style = MaterialTheme.typography.bodySmall, color = if (row.needsInput > 0) stateColors.needs else MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

private val GUIDE_WIDTH = 20.dp

@Composable
private fun AgentLine(row: AgentRow, onClick: () -> Unit) {
    val a: Agent = row.agent
    val line = MaterialTheme.colorScheme.outline
    val gutter = GUIDE_WIDTH * row.depth
    Row(
        Modifier.fillMaxWidth().heightIn(min = 72.dp).clickable(onClick = onClick)
            .drawBehind {
                // The tree's lines: a vertical for each ancestor with later siblings, an elbow or tee to this row.
                val w = GUIDE_WIDTH.toPx()
                val s = Stroke(1.dp.toPx())
                val base = 16.dp.toPx() + w / 2
                for ((i, more) in row.guides.withIndex()) if (more) drawLine(line, Offset(base + w * (i + 1), 0f), Offset(base + w * (i + 1), size.height), s.width)
                if (row.depth > 0) {
                    val x = base + w * row.depth
                    drawLine(line, Offset(x, 0f), Offset(x, if (row.last) size.height / 2 else size.height), s.width)
                    drawLine(line, Offset(x, size.height / 2), Offset(x + w * 0.6f, size.height / 2), s.width)
                }
            }
            .padding(start = 16.dp + gutter + (if (row.depth > 0) 0.dp else 20.dp), end = 16.dp, top = 8.dp, bottom = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.padding(start = if (row.depth > 0) GUIDE_WIDTH else 0.dp)) { StateIcon(a.state, a.exitCode, a.stateDetail, statusSentence(a)) }
        Column(Modifier.padding(start = 12.dp).weight(1f)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(a.name, style = MaterialTheme.typography.bodyLarge, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
                Text(roleLabel(a.role), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Text(statusSentence(a), style = MaterialTheme.typography.bodySmall, color = if (a.needsInput > 0) stateColors.needs else MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
            Text(
                listOf(repoName(a.repo), a.kind, a.model).filter { it.isNotEmpty() }.joinToString(" · "),
                style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis,
            )
        }
    }
}
