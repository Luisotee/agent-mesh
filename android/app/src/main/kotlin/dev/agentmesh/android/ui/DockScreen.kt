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
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.Tile
import dev.agentmesh.android.derive.activityMillis
import dev.agentmesh.android.derive.ago
import dev.agentmesh.android.derive.dockTiles
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.sentLabel
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.AgentState

/**
 * The live dock: a tile for every live agent of the host's open runs, the ones that need you first, each with the tool
 * it runs now and the last thing it said. The agents' state and the ephemeral activity frames feed it; a tap opens the
 * agent. (The desktop's dock; here per host, as the phone opens one host at a time.)
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DockScreen(runtime: HostRuntime, onBack: () -> Unit, onAgent: (String, Tab) -> Unit) {
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
            ConnectionBanner(state, onRetry = runtime::nudge, onLog = {})
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
private fun TileCard(t: Tile, skewMs: Long, now: Long, onClick: () -> Unit) {
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
                        listOfNotNull(t.run?.title?.ifEmpty { t.run.id }, repoName(a.repo).ifEmpty { null }).joinToString(" · "),
                        style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis,
                    )
                }
                Text(ago(activityMillis(a) - skewMs, now), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Text(
                if (t.activity.isNotEmpty()) "Working: ${t.activity}" else statusSentence(a),
                Modifier.testTag("tile-status"), style = MaterialTheme.typography.bodyMedium, color = if (attention) stateColors.needs else MaterialTheme.colorScheme.onSurface,
                maxLines = 2, overflow = TextOverflow.Ellipsis,
            )
            t.lastSent?.let { m ->
                Text("${sentLabel(m)} · ${ago(if (m.hasCreatedAt()) m.createdAt.toMillis() - skewMs else 0, now)}", Modifier.testTag("tile-sent"), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}
