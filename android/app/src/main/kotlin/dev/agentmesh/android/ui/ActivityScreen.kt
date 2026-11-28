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
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ActivityItem
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.flowOf
import java.text.DateFormat
import java.util.Date

/** One row of the Activity list, on the host it came from. */
data class ActivityRow(val hostId: String, val hostName: String, val item: ActivityItem)

/** What each kind of notice is called in the list (the desktop's words). */
val ACTIVITY_KINDS = mapOf(
    "input" to "Needs you", "plan" to "Plan", "question" to "Question", "report" to "Report", "failed" to "Failed", "exited" to "Exited",
    "limited" to "Usage limit", "error" to "Error", "finished" to "Finished", "ready" to "Ready to merge",
)

/** The rows of every host, newest first (by the host's clock, then the event's number). */
fun activityRows(rows: List<ActivityRow>): List<ActivityRow> = rows.sortedWith(compareByDescending<ActivityRow> { it.item.atMs }.thenByDescending { it.item.seq })

/**
 * Activity: what the hosts decided to tell the user (`UserNotice` events since the app connected), the ones that were for
 * them and the ones that only informed (a worker's plan to a lead that decides it). The desktop's Activity list. A tap opens
 * the agent; looking at the list counts everything on it as read (the drawer's count).
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ActivityScreen(onBack: () -> Unit, onOpen: (hostId: String, agentId: String, tab: Tab) -> Unit) {
    val container = LocalContainer.current
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    val hosts by container.hosts.hosts.collectAsStateWithLifecycleCompat()
    val rows by remember(runtimes) {
        if (runtimes.isEmpty()) flowOf(emptyList())
        else combine(runtimes.values.map { rt -> rt.state }) { states ->
            runtimes.values.toList().flatMapIndexed { i, rt -> states[i].activity.map { ActivityRow(rt.hostId, rt.host?.name ?: rt.hostId, it) } }
        }
    }.collectAsStateWithLifecycleCompat(emptyList())
    // Open, everything shown counts as read.
    LaunchedEffect(rows) { rows.groupBy { it.hostId }.forEach { (h, r) -> container.prefs.markActivitySeen(h, r.maxOf { it.item.seq }) } }
    val shown = remember(rows) { activityRows(rows) }
    val many = hosts.size > 1
    Scaffold(topBar = { TopAppBar(title = { Text("Activity") }, navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } }) }) { pad ->
        if (shown.isEmpty()) {
            Text("Nothing yet: what the hosts tell you about appears here.", Modifier.inScaffold(pad).padding(16.dp).testTag("activity-empty"), color = MaterialTheme.colorScheme.onSurfaceVariant)
        } else LazyColumn(Modifier.inScaffold(pad).fillMaxSize().testTag("activity")) {
            items(shown, key = { "${it.hostId}:${it.item.seq}" }) { r ->
                val n = r.item.notice
                Column(Modifier.fillMaxWidth().clickable(enabled = n.agentId.isNotEmpty()) { onOpen(r.hostId, n.agentId, if (n.tab == "terminal") Tab.Terminal else Tab.Chat) }.padding(horizontal = 16.dp, vertical = 10.dp).testTag("activity-row")) {
                    Text(n.title, style = MaterialTheme.typography.bodyLarge, fontWeight = FontWeight.Medium, maxLines = 2, overflow = TextOverflow.Ellipsis)
                    if (n.body.isNotEmpty()) Text(n.body, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
                    Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                        Text(
                            buildString {
                                append(ACTIVITY_KINDS[n.kind] ?: n.kind)
                                if (n.forUser) append(" · for you")
                                if (r.item.atMs > 0) append(" · ").append(DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(r.item.atMs)))
                                if (many) append(" · ").append(r.hostName)
                            },
                            style = MaterialTheme.typography.labelMedium, color = if (n.forUser) stateColors.needs else MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                HorizontalDivider()
            }
        }
    }
}
