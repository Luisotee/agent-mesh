package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.chat.say
import dev.agentmesh.gen.agentmesh.v1.PullRequest
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch

/** "open · checks passing", for a PR's row. */
fun prSummary(p: PullRequest): String = buildString {
    append(if (p.draft && p.state == "open") "draft" else p.state)
    when (p.checks) {
        "pass" -> append(" · checks passing")
        "failing" -> append(" · checks failing")
        "pending" -> append(" · checks running")
    }
    if (p.stale) append(" · not read lately")
    if (p.held) append(" · merge held")
}

/**
 * The pull requests of a change worker, with what the user can do about each: look at it on GitHub, and hold its merge (or
 * release the hold), so a root never merges it until the user says. Shown only for an agent that has some.
 */
@Composable
fun PullRequestsSection(runtime: HostRuntime, agentId: String, canAct: Boolean) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val prs = state.pullRequests[agentId] ?: return
    if (prs.pullRequestsCount == 0) return
    Column(Modifier.fillMaxWidth().testTag("pull-requests")) {
        SectionHeader("Pull requests")
        prs.pullRequestsList.forEach { PrRow(runtime, it, canAct) }
    }
}

@Composable
private fun PrRow(runtime: HostRuntime, pr: PullRequest, canAct: Boolean) {
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    val uri = LocalUriHandler.current
    fun toggle() {
        if (busy) return
        busy = true
        error = ""
        scope.launch {
            try {
                runtime.awaitApi().holdPullRequest(pr.url, release = pr.held)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                // The host's words: a root may not release a hold, a PR it doesn't know.
                error = say(e)
            } finally {
                busy = false
            }
        }
    }
    Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp).testTag("pr-row")) {
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Column(Modifier.weight(1f)) {
                Text("#${pr.number}", style = MaterialTheme.typography.bodyMedium)
                Text(prSummary(pr), style = MaterialTheme.typography.bodySmall, color = if (pr.checks == "failing") MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant)
            }
            if (pr.url.startsWith("https://")) TextButton(onClick = { uri.openUri(pr.url) }) { Text("Open") }
            if (canAct && pr.state == "open") {
                OutlinedButton(onClick = ::toggle, enabled = !busy, modifier = Modifier.testTag("hold")) { Text(if (pr.held) "Release" else "Hold merge") }
            }
        }
        if (error.isNotEmpty()) Text(error, Modifier.testTag("hold-error"), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
    }
}
