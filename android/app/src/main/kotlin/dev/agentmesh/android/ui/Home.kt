package dev.agentmesh.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.derive.LeadRow
import dev.agentmesh.android.derive.WaitingKind
import dev.agentmesh.android.derive.allWorkers
import dev.agentmesh.android.derive.helpersLabel
import dev.agentmesh.android.derive.hueOf
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.notify.Tab

/**
 * Home: what needs the user, the live runs of every host as their leads, and where to start a lead. The drawer holds
 * the projects and everything else; at a wide width it is always open, and the menu button goes.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HomeScreen(
    model: SidebarModel, onMenu: (() -> Unit)?, onAgent: (hostId: String, agentId: String, Tab) -> Unit, onLive: () -> Unit,
    onNewLead: (hostId: String) -> Unit, onPair: () -> Unit, onAdd: () -> Unit, onAddAgent: (hostId: String, runId: String) -> Unit = { _, _ -> },
) {
    val container = LocalContainer.current
    val writable = model.hosts.filter { !it.readOnly }
    var pick by remember { mutableStateOf(false) }
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("agent-mesh") },
                navigationIcon = { if (onMenu != null) IconButton(onClick = onMenu, Modifier.testTag("open-drawer")) { Icon(Icons.Default.Menu, "Menu") } },
                actions = { if (model.hosts.isNotEmpty()) TextButton(onClick = onLive, Modifier.testTag("live-all")) { Text("Live") } },
            )
        },
        floatingActionButton = {
            if (writable.isNotEmpty()) Box {
                ExtendedFloatingActionButton(onClick = { if (writable.size == 1) onNewLead(writable[0].hostId) else pick = true }, Modifier.testTag("new-run")) { Text("New lead") }
                DropdownMenu(pick, { pick = false }) {
                    writable.forEach { h -> DropdownMenuItem(text = { Text("on ${h.name}") }, onClick = { pick = false; onNewLead(h.hostId) }) }
                }
            }
        },
    ) { pad ->
        if (model.hosts.isEmpty()) {
            EmptyHosts(Modifier.padding(pad), container.hosts.recoveredFrom != null, onAdd, onPair)
            return@Scaffold
        }
        val live = model.runs
        LazyColumn(Modifier.padding(pad).fillMaxSize(), contentPadding = PaddingValues(bottom = 88.dp)) {
            model.hosts.filter { !it.up && it.note.isNotEmpty() }.forEach { h ->
                item(key = "off:${h.hostId}") {
                    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp).testTag("host-offline"), verticalAlignment = Alignment.CenterVertically) {
                        Text("${h.name}: ${h.note}", Modifier.weight(1f), style = MaterialTheme.typography.bodySmall, color = stateColors.needs, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        TextButton(onClick = { container.runtimes.value[h.hostId]?.nudge() }) { Text("Retry") }
                    }
                }
            }
            if (model.loading) item(key = "loading") { SkeletonRows("Loading the runs", Modifier.padding(16.dp), rows = 4) }
            if (model.waiting.isNotEmpty()) {
                item(key = "waiting-header") { SectionHeader("Waiting on you (${model.waiting.size})") }
                items(model.waiting.size, key = { i -> model.waiting[i].let { "wait:${it.runtime.hostId}:${it.w.kind}:${it.w.message?.id ?: it.w.agent.id}" } }) { i ->
                    val w = model.waiting[i]
                    WaitingRow(w.runtime, w.w, canAct = model.hosts.none { it.hostId == w.runtime.hostId && it.readOnly }) {
                        onAgent(w.runtime.hostId, w.w.agent.id, if (w.w.kind == WaitingKind.Input) Tab.Terminal else Tab.Messages)
                    }
                }
            }
            if (live.isNotEmpty()) item(key = "live-header") { SectionHeader("Live runs") }
            items(live, key = { "run:${it.hostId}:${it.run.id}" }) { r ->
                val canAdd = r.run.status == dev.agentmesh.gen.agentmesh.v1.RunStatus.RUN_STATUS_OPEN && model.hosts.none { h -> h.hostId == r.hostId && h.readOnly }
                LeadCard(r, model.many, model.hosts.firstOrNull { it.hostId == r.hostId }?.name.orEmpty(), canAdd) {
                    // A run with no agent yet offers to start one (when this device may).
                    val lead = r.lead
                    if (lead != null) onAgent(r.hostId, lead.id, Tab.Chat)
                    else if (canAdd) onAddAgent(r.hostId, r.run.id)
                }
            }
            if (live.isEmpty() && !model.loading && model.waiting.isEmpty() && model.hosts.any { !it.loading }) item(key = "empty") {
                Text("No live runs. Start a lead with New lead, or from the host's CLI.", Modifier.padding(16.dp).testTag("home-empty"), color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}

/** A live run as its lead: the project in its colour, what the lead is doing, and its workers and reviewers folded in. */
@Composable
private fun LeadCard(r: LeadRow, withHost: Boolean, hostName: String, canAdd: Boolean, onClick: () -> Unit) {
    val lead = r.lead
    val hue = LocalMeshTheme.current.hues[hueOf(lead?.repo.orEmpty()) - 1]
    val attention = r.needs > 0
    Card(
        Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp).clickable(enabled = lead != null || canAdd, onClick = onClick).testTag("run-card"),
        colors = CardDefaults.cardColors(containerColor = if (attention) MaterialTheme.colorScheme.surfaceContainerHigh else MaterialTheme.colorScheme.surfaceContainer),
    ) {
        Row(Modifier.padding(14.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            if (lead != null) StateIcon(lead.state, lead.exitCode, lead.stateDetail, statusSentence(lead), size = 22.dp)
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                Text(r.title, style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text(
                    listOfNotNull(repoName(lead?.repo.orEmpty()).ifEmpty { null }, hostName.takeIf { withHost && it.isNotEmpty() }).joinToString(" · "),
                    style = MaterialTheme.typography.labelMedium, color = hue, maxLines = 1, overflow = TextOverflow.Ellipsis,
                )
                Text(
                    lead?.let(::statusSentence) ?: "No agent yet", style = MaterialTheme.typography.bodySmall, maxLines = 2, overflow = TextOverflow.Ellipsis,
                    color = if (attention) stateColors.needs else MaterialTheme.colorScheme.onSurfaceVariant,
                )
                val workers = allWorkers(r.workers).size
                val extras = listOfNotNull(
                    if (workers > 0) "$workers ${if (workers == 1) "worker" else "workers"}" else null,
                    helpersLabel(r.helpers + allWorkers(r.workers).flatMap { it.helpers }).ifEmpty { null },
                )
                if (extras.isNotEmpty()) Text(extras.joinToString(" · "), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            if (lead != null) VendorIcon(lead.kind, lead.model, size = 18.dp)
        }
    }
}
