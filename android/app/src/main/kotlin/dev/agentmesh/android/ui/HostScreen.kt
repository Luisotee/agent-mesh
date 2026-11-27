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
import androidx.compose.material3.FilterChip
import androidx.compose.material3.TextButton
import androidx.compose.ui.platform.testTag
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
import androidx.compose.runtime.saveable.listSaver
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
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.ui.chat.isReadOnlyDevice
import dev.agentmesh.android.ui.messages.AnswerForm
import dev.agentmesh.android.ui.messages.MarkdownText
import dev.agentmesh.android.ui.messages.isLong
import dev.agentmesh.android.ui.messages.preview
import dev.agentmesh.android.derive.RunRow
import dev.agentmesh.android.derive.Waiting
import dev.agentmesh.android.derive.WaitingKind
import dev.agentmesh.android.derive.buildTrees
import dev.agentmesh.android.derive.flatten
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.roleLabel
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.derive.BucketContext
import dev.agentmesh.android.derive.groupByState
import dev.agentmesh.android.derive.waitingFor
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.RunStatus

/** A host's home: its connection, what waits on the user, and each run as a tree from its lead. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HostScreen(runtime: HostRuntime, onBack: () -> Unit, onAgent: (String, Tab) -> Unit, onLog: () -> Unit, onSettings: () -> Unit, onDock: () -> Unit = {}, onUsage: () -> Unit = {}) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val trees = remember(state.runs, state.agents) { buildTrees(state.runs, state.agents) }
    val waiting = remember(state.waiting, state.runs, state.agents, state.messages) { waitingFor(state) }
    // Runs as trees, or the agents of the open runs by what they need: the user's choice, kept across rotation.
    var byStatus by rememberSaveable { mutableStateOf(false) }
    val buckets = remember(state.runs, state.agents, waiting, state.pullRequests) {
        groupByState(state.runs, state.agents, BucketContext(waiting.mapTo(HashSet()) { it.agent.id }, state.pullRequests))
    }
    val readOnly = isReadOnlyDevice(state)
    // A new run, or an agent added to a run: only for a device that may act.
    var sheet by rememberSaveable { mutableStateOf<String?>(null) } // "" a new run, else the run to add an agent to
    // Closed runs start collapsed and open ones expanded; the ids the user flipped are kept across rotation.
    var flipped by rememberSaveable(stateSaver = listSaver<List<String>, String>(save = { it }, restore = { it })) { mutableStateOf(emptyList<String>()) }
    val rows = remember(trees, flipped) { flatten(trees) { r -> (r.status == RunStatus.RUN_STATUS_OPEN) != (r.id in flipped) } }
    Scaffold(floatingActionButton = {
        if (!readOnly) androidx.compose.material3.ExtendedFloatingActionButton(onClick = { sheet = "" }, modifier = Modifier.testTag("new-run")) { Text("New run") }
    }, topBar = {
        TopAppBar(
            title = { Text(runtime.host?.name ?: "Host") },
            navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } },
            actions = {
                TextButton(onClick = onDock, modifier = Modifier.testTag("open-dock")) { Text("Live") }
                TextButton(onClick = onUsage, modifier = Modifier.testTag("open-usage")) { Text("Usage") }
                IconButton(onClick = onSettings) { Icon(Icons.Default.Settings, "Host settings") }
            },
        )
    }) { pad ->
        Column(Modifier.padding(pad).fillMaxSize()) {
            ConnectionBanner(state, onRetry = runtime::nudge, onLog = onLog)
            LazyColumn(Modifier.fillMaxSize()) {
                if (waiting.isNotEmpty()) {
                    item(key = "waiting-header") { SectionHeader("Waiting on you (${waiting.size})") }
                    items(waiting, key = { "wait:${it.kind}:${it.message?.id ?: it.agent.id}" }, contentType = { "waiting" }) { w ->
                        WaitingRow(runtime, w, canAct = !readOnly) { onAgent(w.agent.id, if (w.kind == WaitingKind.Input) Tab.Terminal else Tab.Messages) }
                    }
                    item(key = "waiting-divider") { HorizontalDivider(Modifier.padding(top = 8.dp)) }
                }
                if (rows.isEmpty() && state.synced) {
                    item(key = "empty") {
                        Text("No runs yet. Start one from the desktop app or the host's CLI.", Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                item(key = "view") {
                    Row(Modifier.padding(horizontal = 16.dp, vertical = 4.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        FilterChip(selected = !byStatus, onClick = { byStatus = false }, label = { Text("Runs") }, modifier = Modifier.testTag("view-runs"))
                        FilterChip(selected = byStatus, onClick = { byStatus = true }, label = { Text("By status") }, modifier = Modifier.testTag("view-status"))
                    }
                }
                if (byStatus) {
                    buckets.forEach { g ->
                        item(key = "bucket:${g.bucket}") { SectionHeader("${g.bucket.label} (${g.agents.size})", Modifier.testTag("bucket-${g.bucket.name}")) }
                        items(g.agents, key = { "b:${g.bucket}:${it.id}" }, contentType = { "agent" }) { a ->
                            AgentLine(AgentRow(a, 0, emptyList(), true)) { onAgent(a.id, Tab.Overview) }
                        }
                    }
                } else items(rows, key = { it.key }, contentType = { if (it is RunRow) "run" else "agent" }) { row ->
                    when (row) {
                        is RunRow -> RunHeader(row, onAddAgent = if (!readOnly && row.run.status == RunStatus.RUN_STATUS_OPEN) ({ sheet = row.run.id }) else null) {
                            val id = row.run.id
                            flipped = if (id in flipped) flipped - id else flipped + id
                        }
                        is AgentRow -> AgentLine(row) { onAgent(row.agent.id, Tab.Overview) }
                    }
                }
            }
        }
    }
    sheet?.let { target -> SpawnSheet(runtime, runId = target.ifEmpty { null }, onDismiss = { sheet = null }) }
}

/**
 * One thing that waits on the user. A plan or a question opens in place (what it says, and the Approve or the answer),
 * so the user needn't leave the list; a prompt in a terminal opens the agent's terminal.
 */
@Composable
private fun WaitingRow(runtime: HostRuntime, w: Waiting, canAct: Boolean, onOpen: () -> Unit) {
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

@Composable
private fun RunHeader(row: RunRow, onAddAgent: (() -> Unit)?, onToggle: () -> Unit) {
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
        if (onAddAgent != null) androidx.compose.material3.TextButton(onClick = onAddAgent, modifier = Modifier.testTag("add-agent-${row.run.id}")) { Text("+ Agent") }
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
