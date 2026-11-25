package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.AssistChip
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.term.TermClient
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.term.TermState

fun termSentence(s: TermState): String = when (s) {
    TermState.Attaching -> "Attaching…"
    TermState.Live -> "Live"
    is TermState.Reconnecting -> "Reconnecting (${s.attempt}): ${s.why}"
    TermState.Ended -> "The agent's process ended"
    is TermState.Exited -> if (s.signal != 0) "The agent's process was stopped (signal ${s.signal})" else if (s.exitCode == 0) "The agent's process ended" else "The agent's process exited with code ${s.exitCode}"
    is TermState.Failed -> s.message
}

/**
 * An agent's terminal, read only: the host's screen as the agent's CLI draws it, live, with the history to scroll
 * through. It attaches while this tab shows and lets go when it doesn't, and never types (docs/apps.md puts typing
 * and a key row after the first version).
 */
@Composable
fun TerminalTab(runtime: HostRuntime, agentId: String, modifier: Modifier = Modifier) {
    val scope = rememberCoroutineScope()
    val screen = remember(agentId) { TermScreen() }
    val client = remember(agentId) { TermClient(scope, agentId, { runtime.awaitApi() }, screen) }
    DisposableEffect(client) {
        client.start()
        onDispose { client.stop() }
    }
    val state by client.state.collectAsStateWithLifecycleCompat()
    Column(modifier.fillMaxSize()) {
        Row(Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            AssistChip(onClick = {}, label = { Text("Read only") })
            Text(
                termSentence(state), Modifier.weight(1f).testTag("terminal-state"), style = MaterialTheme.typography.bodySmall,
                color = if (state is TermState.Failed) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Box(Modifier.weight(1f).fillMaxWidth()) {
            TerminalCanvas(screen, LocalMeshTheme.current.terminal)
            if (screen.scrollBack > 0) {
                Button(onClick = { screen.scrollBack = 0 }, modifier = Modifier.align(Alignment.BottomEnd).padding(12.dp)) { Text("Latest") }
            }
        }
    }
}
