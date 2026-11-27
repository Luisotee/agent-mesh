package dev.agentmesh.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.derive.LeadRow
import dev.agentmesh.android.derive.allWorkers
import dev.agentmesh.android.derive.anyWorking
import dev.agentmesh.android.derive.helpersLabel
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.gen.agentmesh.v1.Agent

/** The agents the strip lists: the lead, then its workers at any depth; [current] is added when it is neither (a reviewer opened from the details). */
fun stripAgents(row: LeadRow, current: Agent): List<Agent> {
    val tree = listOfNotNull(row.lead) + allWorkers(row.workers).map { it.agent }
    return if (tree.any { it.id == current.id }) tree else tree + current
}

/** Every reviewer and asker of the run, folded under their agent: what the strip's last chip counts. */
fun stripHelpers(row: LeadRow): List<Agent> = row.helpers + allWorkers(row.workers).flatMap { it.helpers }

/**
 * The open agent's tree as a strip under the top bar: its lead and workers, each with its live state, the open one marked;
 * reviewers and askers as one chip ("·2 reviews") that opens the details. A tap switches agent in place, so another lead's
 * strip is its own. Nothing shows for a lone agent: there is nothing to switch to.
 */
@Composable
fun TreeStrip(row: LeadRow?, current: Agent, onOpen: (Agent) -> Unit, onHelpers: () -> Unit, modifier: Modifier = Modifier) {
    if (row == null) return
    val agents = stripAgents(row, current)
    val helpers = stripHelpers(row)
    if (agents.size < 2 && helpers.isEmpty()) return
    Row(modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = 12.dp, vertical = 6.dp).testTag("tree-strip"), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
        agents.forEach { a ->
            val here = a.id == current.id
            Surface(
                shape = CircleShape, color = if (here) MaterialTheme.colorScheme.secondaryContainer else MaterialTheme.colorScheme.surfaceContainer,
                modifier = Modifier.clickable(enabled = !here) { onOpen(a) }.semantics { contentDescription = "${a.name}, ${statusSentence(a)}" }.testTag("tree-${a.id}"),
            ) {
                Row(Modifier.padding(start = 10.dp, end = 12.dp, top = 6.dp, bottom = 6.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    StateIcon(a.state, a.exitCode, a.stateDetail, statusSentence(a), size = 14.dp)
                    Text(a.name, style = MaterialTheme.typography.labelLarge, maxLines = 1, overflow = TextOverflow.Ellipsis)
                }
            }
        }
        if (helpers.isNotEmpty()) {
            Surface(shape = CircleShape, color = MaterialTheme.colorScheme.surfaceContainer, modifier = Modifier.clickable(onClick = onHelpers).testTag("tree-helpers")) {
                Text(
                    "·${helpersLabel(helpers)}", Modifier.padding(horizontal = 12.dp, vertical = 6.dp), style = MaterialTheme.typography.labelLarge,
                    color = if (anyWorking(helpers)) stateColors.working else MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1,
                )
            }
        }
    }
}
