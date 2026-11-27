package dev.agentmesh.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.DockHost
import dev.agentmesh.android.derive.Tile
import dev.agentmesh.android.derive.dockTilesAcross
import dev.agentmesh.android.derive.activityAgo
import dev.agentmesh.android.derive.sentAgo
import dev.agentmesh.android.derive.dockTiles
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.sentLabel
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.gen.agentmesh.v1.AgentState

/**
 * The live dock: a tile for every live agent of the host's open runs, the ones that need you first, each with the tool
 * it runs now and the last thing it said. The agents' state and the ephemeral activity frames feed it; a tap opens the
 * agent. (The desktop's dock; here per host, as the phone opens one host at a time.)
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DockScreen(runtime: HostRuntime, onBack: () -> Unit, onLog: () -> Unit = {}, onAgent: (String, Tab) -> Unit) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val tiles = remember(state.runs, state.agents, state.messages) { dockTiles(state.runs, state.agents, state.messages) }
    val now = rememberNowMs(true)
    Scaffold(topBar = {
        TopAppBar(
            title = { Text("Live · ${runtime.host?.name ?: "Host"}") },
            navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } },
        )
    }) { pad ->
        Column(Modifier.padding(pad).fillMaxSize()) {
            ConnectionBanner(state, onRetry = runtime::nudge, onLog = onLog)
            if (tiles.isEmpty()) {
                Text(
                    if (state.synced) "No live agents. Agents of an open run show here while they work." else "Waiting for the host…",
                    Modifier.padding(16.dp).testTag("dock-empty"), color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            LazyColumn(Modifier.fillMaxSize(), contentPadding = androidx.compose.foundation.layout.PaddingValues(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                items(tiles, key = { it.agent.id }) { t -> TileCard(t, state.clockSkewMs, now) { onAgent(t.agent.id, if (t.agent.state == AgentState.AGENT_STATE_NEEDS_INPUT) Tab.Terminal else Tab.Chat) } }
            }
        }
    }
}

@Composable
internal fun TileCard(t: Tile, skewMs: Long, now: Long, onClick: () -> Unit) {
    val a = t.agent
    val attention = a.state == AgentState.AGENT_STATE_NEEDS_INPUT || a.state == AgentState.AGENT_STATE_ERROR || a.state == AgentState.AGENT_STATE_LIMITED
    Card(
        Modifier.fillMaxWidth().clickable(onClick = onClick).testTag("tile"),
        colors = CardDefaults.cardColors(containerColor = if (attention) MaterialTheme.colorScheme.surfaceContainerHigh else MaterialTheme.colorScheme.surfaceContainer),
    ) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                StateIcon(a.state, a.exitCode, a.stateDetail, statusSentence(a))
                Column(Modifier.weight(1f)) {
                    Text(a.name, style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    Text(
                        listOfNotNull(t.hostName.ifEmpty { null }, t.run?.title?.ifEmpty { t.run.id }, repoName(a.repo).ifEmpty { null }).joinToString(" · "),
                        style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis,
                    )
                }
                Text(activityAgo(a, skewMs, now), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Text(
                if (t.activity.isNotEmpty()) "Working: ${t.activity}" else statusSentence(a),
                Modifier.testTag("tile-status"), style = MaterialTheme.typography.bodyMedium, color = if (attention) stateColors.needs else MaterialTheme.colorScheme.onSurface,
                maxLines = 2, overflow = TextOverflow.Ellipsis,
            )
            t.lastSent?.let { m ->
                Text(listOf(sentLabel(m), sentAgo(m, skewMs, now)).filter { it.isNotEmpty() }.joinToString(" · "), Modifier.testTag("tile-sent"), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}

/**
 * The live dock of every host at once: a tile for each live agent of each host's open runs, the ones that need you first,
 * with the host's name on each. A tap opens the agent on its host. A host that isn't connected shows what it last said, and
 * a banner above says how many are not reachable.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AllHostsDockScreen(onBack: () -> Unit, onAgent: (hostId: String, agentId: String, tab: Tab) -> Unit) {
    val container = LocalContainer.current
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    // A host's name is the list's, which changes on a rename without any host's state changing.
    val hostList by container.hosts.hosts.collectAsStateWithLifecycleCompat()
    val list = remember(runtimes) { runtimes.values.toList() }
    // Every host's state in one list: it changes when any of them does. Collected only while the screen is on show, and from what
    // the hosts hold now, so there is no empty frame before the first answer.
    val statesFlow = remember(list) {
        if (list.isEmpty()) kotlinx.coroutines.flow.flowOf(emptyList<Pair<HostRuntime, dev.agentmesh.android.store.HostState>>())
        else kotlinx.coroutines.flow.combine(list.map { rt -> rt.state }) { arr -> list.zip(arr.toList()) }
    }
    // The current values are only the first frame's: after it the flow is what is read.
    @Suppress("StateFlowValueCalledInComposition")
    val first = list.map { it to it.state.value }
    val states by statesFlow.collectAsStateWithLifecycleCompat(first)
    val tiles = remember(states, hostList) {
        dockTilesAcross(states.map { (rt, st) -> DockHost(rt.hostId, hostList.firstOrNull { it.id == rt.hostId }?.name ?: rt.host?.name ?: "", st.clockSkewMs, st.runs, st.agents, st.messages) })
    }
    val unreachable = states.count { (_, st) -> st.conn !is dev.agentmesh.android.store.ConnState.Ready }
    val now = rememberNowMs(true)
    Scaffold(topBar = {
        TopAppBar(
            title = { Text("Live · all hosts") },
            navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } },
        )
    }) { pad ->
        Column(Modifier.padding(pad).fillMaxSize()) {
            if (unreachable > 0) Text(
                "$unreachable of ${states.size} hosts not connected: their tiles show what they last said.",
                Modifier.padding(horizontal = 16.dp, vertical = 8.dp).testTag("dock-unreachable"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (tiles.isEmpty()) {
                Text(
                    if (states.isEmpty()) "No hosts yet." else "No live agents on any host. Agents of an open run show here while they work.",
                    Modifier.padding(16.dp).testTag("dock-empty"), color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            LazyColumn(Modifier.fillMaxSize(), contentPadding = androidx.compose.foundation.layout.PaddingValues(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                items(tiles, key = { it.hostId + "/" + it.agent.id }) { t ->
                    TileCard(t, t.skewMs, now) { onAgent(t.hostId, t.agent.id, if (t.agent.state == AgentState.AGENT_STATE_NEEDS_INPUT) Tab.Terminal else Tab.Chat) }
                }
            }
        }
    }
}
