package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.ui.chat.isReadOnlyDevice
import kotlinx.coroutines.launch

/**
 * A project shell: a plain login shell in the project's folder on the host, which keeps running when the app is closed. The
 * screen is its terminal, with a key row and the keyboard on, and ⋯ to end it (Back only leaves it running: it is still in the
 * drawer under its project). A shell the phone opened is sized to this view, so what it prints wraps at the phone's width.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ShellScreen(runtime: HostRuntime, shellId: String, onBack: () -> Unit, onMenu: (() -> Unit)? = null) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val shell = state.shells?.firstOrNull { it.id == shellId }
    // The last output stays while the screen is open even after the host forgot the shell (it ended and was listed again).
    var seen by remember(shellId) { mutableStateOf(false) }
    LaunchedEffect(shell != null) { if (shell != null) seen = true }
    // A shell the host no longer lists is gone: the phone's mark of it goes too.
    val marks = LocalContainer.current.prefs
    LaunchedEffect(state.shells != null, shell == null) { if (state.shells != null && shell == null) marks.forgetPhoneShell(runtime.hostId, shellId) }
    var menu by remember { mutableStateOf(false) }
    var ending by remember { mutableStateOf(false) }
    val canAct = !isReadOnlyDevice(state)
    val prefs = LocalContainer.current.prefs
    val project = shell?.let { repoName(it.repo) }.orEmpty()
    Scaffold(topBar = {
        TopAppBar(
            title = {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    Text(">_", fontFamily = FontFamily.Monospace, color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.titleMedium)
                    Column {
                        Text(if (project.isEmpty()) "Terminal" else "Terminal in $project", style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        if (shell != null) Text(shell.repo, Modifier.testTag("shell-path"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                }
            },
            navigationIcon = {
                if (onMenu != null) IconButton(onClick = onMenu, Modifier.testTag("open-drawer")) { Icon(Icons.Default.Menu, "Menu") }
                else IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") }
            },
            actions = {
                if (shell != null && !shell.exited && canAct) {
                    IconButton(onClick = { menu = true }, Modifier.testTag("shell-menu")) { Icon(Icons.Default.MoreVert, "Menu") }
                    DropdownMenu(menu, { menu = false }) {
                        DropdownMenuItem(text = { Text("End the terminal…") }, onClick = { menu = false; ending = true }, modifier = Modifier.testTag("shell-end"))
                    }
                }
            },
        )
    }) { pad ->
        when {
            shell != null || seen -> TerminalTab(runtime, shellId, Modifier.inScaffold(pad).fillMaxSize(), shell = true, phoneSized = prefs.isPhoneShell(runtime.hostId, shellId))
            state.shells == null && state.shellsFailed -> Column(Modifier.padding(pad).padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("The host's terminals could not be read.", Modifier.testTag("shell-unread"), color = MaterialTheme.colorScheme.error)
                val scope = androidx.compose.runtime.rememberCoroutineScope()
                TextButton(onClick = { scope.launch { runtime.refreshShells() } }) { Text("Try again") }
            }
            state.shells == null -> Text("Connecting to the host…", Modifier.padding(pad).padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
            else -> Column(Modifier.padding(pad).padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("That terminal has ended.", Modifier.testTag("shell-gone"), color = MaterialTheme.colorScheme.onSurfaceVariant)
                TextButton(onClick = onBack) { Text("Back") }
            }
        }
    }
    if (ending) AgentActionDialog(runtime, AgentAction.EndShell(shellId, project)) { ok -> ending = false; if (ok) onBack() }
}
