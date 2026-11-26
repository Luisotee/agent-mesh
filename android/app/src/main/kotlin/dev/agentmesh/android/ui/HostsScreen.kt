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
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Checkbox
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.HorizontalDivider
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
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.waitingOnYou
import dev.agentmesh.android.hosts.Host
import dev.agentmesh.android.hosts.UnpairResult
import dev.agentmesh.android.hosts.unpairFromHost
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import kotlinx.coroutines.launch

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HostsScreen(onOpen: (String) -> Unit, onAdd: () -> Unit, onPair: () -> Unit, onEdit: (String) -> Unit, onSettings: () -> Unit) {
    val container = LocalContainer.current
    val hosts by container.hosts.hosts.collectAsStateWithLifecycleCompat()
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    var removing by remember { mutableStateOf<Host?>(null) }
    Scaffold(
        topBar = { TopAppBar(title = { Text("agent-mesh") }, actions = { IconButton(onClick = onSettings) { Icon(Icons.Default.Settings, "Settings") } }) },
        floatingActionButton = {
            var addMenu by remember { mutableStateOf(false) }
            Column {
                FloatingActionButton(onClick = { addMenu = true }) { Icon(Icons.Default.Add, "Add a host") }
                DropdownMenu(addMenu, { addMenu = false }) {
                    DropdownMenuItem(text = { Text("Pair with a code") }, onClick = { addMenu = false; onPair() })
                    DropdownMenuItem(text = { Text("Add by address") }, onClick = { addMenu = false; onAdd() })
                }
            }
        },
    ) { pad ->
        if (hosts.isEmpty()) {
            EmptyHosts(Modifier.padding(pad), container.hosts.recoveredFrom != null, onAdd, onPair)
        } else {
            LazyColumn(Modifier.padding(pad).fillMaxSize()) {
                items(hosts, key = { it.id }) { h ->
                    HostRow(h, runtimes[h.id], onOpen = { onOpen(h.id) }, onEdit = { onEdit(h.id) }, onRemove = { removing = h })
                    HorizontalDivider()
                }
            }
        }
    }
    removing?.let { h ->
        val runtime = runtimes[h.id]
        val state by (runtime?.state ?: remember { kotlinx.coroutines.flow.MutableStateFlow(dev.agentmesh.android.store.HostState()) }).collectAsStateWithLifecycleCompat()
        val ready = runtime != null && state.conn is ConnState.Ready
        RemoveHostDialog(
            hostName = h.name,
            canUnpair = ready,
            unpair = { unpairFromHost(runtime?.api()) },
            onRemove = { container.hosts.remove(h.id); removing = null },
            onDismiss = { removing = null },
        )
    }
}

/**
 * Removing a host: the app forgets it and its token, and, when connected, can first ask the host to revoke this
 * device's pairing (`RevokeSelf`), so the token doesn't stay valid there. A failed call leaves the choice to the user:
 * try again (which only retries the unpair, and waits for a connection), or remove only here (the host's owner then
 * revokes the device with `mesh revoke-device`): the host is never forgotten without one of those two being chosen.
 */
@Composable
fun RemoveHostDialog(hostName: String, canUnpair: Boolean, unpair: suspend () -> UnpairResult, onRemove: () -> Unit, onDismiss: () -> Unit) {
    val scope = rememberCoroutineScope()
    // Ticked by default: it only counts while the host is connected, so a connection that comes up with the dialog open is covered.
    var alsoUnpair by remember { mutableStateOf(true) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    AlertDialog(
        onDismissRequest = { if (!busy) onDismiss() },
        title = { Text("Remove $hostName?") },
        text = {
            Column {
                Text("The app forgets this host and its token. The host and its agents are untouched.")
                if (error != null) {
                    // After a failure the choice is "Try again" or "Remove here only": no box to untick.
                    if (!canUnpair) Text("Waiting for the host to be reachable again.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 8.dp))
                } else if (canUnpair) {
                    Row(Modifier.clickable(enabled = !busy) { alsoUnpair = !alsoUnpair }, verticalAlignment = Alignment.CenterVertically) {
                        Checkbox(alsoUnpair, { alsoUnpair = it }, enabled = !busy, modifier = Modifier.testTag("also-unpair"))
                        Text("Also unpair this phone from the host")
                    }
                } else {
                    Text(
                        "The host isn't reachable, so this phone's token stays valid on it until its owner revokes the device (mesh revoke-device).",
                        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 8.dp),
                    )
                }
                error?.let { Text("Couldn't unpair: $it", color = MaterialTheme.colorScheme.error, modifier = Modifier.padding(top = 8.dp).testTag("unpair-error")) }
            }
        },
        confirmButton = {
            val retry = error != null
            TextButton(enabled = !busy && (!retry || canUnpair), onClick = {
                // Only a first try may remove without unpairing (box unticked, or no connection): a retry never does.
                if (!retry && !(canUnpair && alsoUnpair)) return@TextButton onRemove()
                busy = true
                error = null
                scope.launch {
                    when (val r = unpair()) {
                        UnpairResult.Done -> onRemove()
                        is UnpairResult.Failed -> { busy = false; error = r.message }
                    }
                }
            }) { Text(if (retry) "Try again" else "Remove") }
        },
        dismissButton = {
            Row {
                if (error != null) TextButton(enabled = !busy, onClick = onRemove) { Text("Remove here only") }
                TextButton(enabled = !busy, onClick = onDismiss) { Text("Cancel") }
            }
        },
    )
}

@Composable
private fun EmptyHosts(modifier: Modifier, recovered: Boolean, onAdd: () -> Unit, onPair: () -> Unit) {
    Column(modifier.fillMaxSize().padding(32.dp), verticalArrangement = Arrangement.Center, horizontalAlignment = Alignment.CenterHorizontally) {
        Text("No hosts yet", style = MaterialTheme.typography.titleMedium)
        Text(
            "A host is a machine running meshd. Pair with it using the code from mesh pair (a QR code or a link), or add it by hand with its address and a device token.",
            style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(vertical = 12.dp),
        )
        if (recovered) Text("The saved host list couldn't be read, so it was set aside (hosts.json.bad in the app's files). Add your hosts again.", color = MaterialTheme.colorScheme.error, modifier = Modifier.padding(bottom = 12.dp))
        androidx.compose.material3.Button(onClick = onPair, modifier = Modifier.testTag("pair-host")) { Text("Pair with a host") }
        androidx.compose.material3.TextButton(onClick = onAdd) { Text("Add by address") }
    }
}

@Composable
private fun HostRow(host: Host, runtime: HostRuntime?, onOpen: () -> Unit, onEdit: () -> Unit, onRemove: () -> Unit) {
    val state by (runtime?.state ?: remember { kotlinx.coroutines.flow.MutableStateFlow(dev.agentmesh.android.store.HostState()) }).collectAsStateWithLifecycleCompat()
    val waiting = remember(state.runs, state.agents, state.messages) { waitingOnYou(state.runs, state.agents, state.messages).size }
    val open = state.runs.values.count { it.status == RunStatus.RUN_STATUS_OPEN }
    var menu by remember { mutableStateOf(false) }
    val now = rememberNowMs(state.conn is ConnState.Reconnecting)
    Row(Modifier.fillMaxWidth().clickable(onClick = onOpen).padding(start = 16.dp, top = 12.dp, bottom = 12.dp, end = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        ConnDot(state.conn)
        Column(Modifier.weight(1f).padding(start = 16.dp)) {
            Text(host.name, style = MaterialTheme.typography.titleMedium)
            Text(host.routes.firstOrNull()?.let(::routeLabel).orEmpty(), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            val sentence = when (val c = state.conn) {
                is ConnState.Ready -> if (state.synced) "$open open runs" + if (waiting > 0) ", $waiting waiting on you" else "" else "Syncing…"
                else -> connSentence(c, now)
            }
            Text(sentence, style = MaterialTheme.typography.bodySmall, color = if (waiting > 0) stateColors.needs else MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Column {
            IconButton(onClick = { menu = true }) { Icon(Icons.Default.MoreVert, "Host menu") }
            DropdownMenu(menu, { menu = false }) {
                DropdownMenuItem(text = { Text("Edit") }, onClick = { menu = false; onEdit() })
                DropdownMenuItem(text = { Text("Remove") }, onClick = { menu = false; onRemove() })
            }
        }
    }
}
