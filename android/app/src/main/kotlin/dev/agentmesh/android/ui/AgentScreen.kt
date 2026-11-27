package dev.agentmesh.android.ui

import androidx.activity.compose.BackHandler
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.ContentTransform
import androidx.compose.animation.EnterTransition
import androidx.compose.animation.ExitTransition
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.core.tween
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.debounce
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.snapshotFlow
import androidx.compose.foundation.text.input.rememberTextFieldState
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.LifecycleResumeEffect
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.SidebarHost
import dev.agentmesh.android.derive.leadRowOf
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.ui.chat.ChatTab
import dev.agentmesh.android.ui.chat.isReadOnlyDevice
import dev.agentmesh.android.ui.messages.SendMessageSheet

/**
 * One agent, chat first: its conversation is the screen, with the mesh's messages to and from it as cards in place. The top
 * bar has its vendor and state, `>_` to swap the chat for its terminal, and ⋯ for the details
 * (pull requests, the agent tree, cost, branch, a message to it, Stop). Under it, the model (a tap changes it) and a strip with
 * the agent's tree: a tap switches agent. [initialTab] only says which view opens: the terminal, or else the chat.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AgentScreen(
    runtime: HostRuntime, agentId: String, onBack: () -> Unit, initialTab: Tab = Tab.Chat, onMenu: (() -> Unit)? = null,
    onAgent: (String) -> Unit = {},
) {
    val container = LocalContainer.current
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val agent = state.agents[agentId]
    val run = agent?.let { state.runs[it.runId] }
    var terminal by rememberSaveable { mutableStateOf(initialTab == Tab.Terminal) }
    var details by rememberSaveable { mutableStateOf(false) }
    var send by rememberSaveable { mutableStateOf(false) }
    // The draft lives here, so switching to the terminal and back keeps it (the chat leaves composition), and in the
    // device's prefs, so leaving the screen or the app does too: a moment after typing stops, and cleared when it is sent.
    val prefs = LocalContainer.current.prefs
    val actionScope = androidx.compose.runtime.rememberCoroutineScope()
    val draft = rememberTextFieldState(remember(runtime.hostId, agentId) { prefs.draft(runtime.hostId, agentId) })
    LaunchedEffect(draft, runtime.hostId, agentId) {
        snapshotFlow { draft.text.toString() }.drop(1).debounce(400).collect { prefs.setDraft(runtime.hostId, agentId, it) }
    }
    DisposableEffect(draft, runtime.hostId, agentId) { onDispose { prefs.setDraft(runtime.hostId, agentId, draft.text.toString()) } }
    // Looking at an agent answers its notification.
    LaunchedEffect(agentId) { container.notifier.cancel(runtime.hostId, agentId) }
    // While it is on screen and the app resumed, nothing new is posted for it (see AppContainer.viewing).
    LifecycleResumeEffect(agentId) {
        val me = runtime.hostId to agentId
        container.viewing.value = me
        container.notifier.cancel(runtime.hostId, agentId)
        onPauseOrDispose { container.viewing.compareAndSet(me, null) }
    }
    // Back from the terminal is the chat; the drawer's handler (registered later) still wins while it is open.
    BackHandler(enabled = terminal) { terminal = false }
    val canAct = !isReadOnlyDevice(state)
    val reduced = LocalReducedMotion.current
    Scaffold(topBar = {
        TopAppBar(
            title = {
                if (agent != null) Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    VendorIcon(agent.kind, agent.model, size = 22.dp)
                    Column {
                        Text(agent.name, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                            StateIcon(agent.state, agent.exitCode, agent.stateDetail, statusSentence(agent), size = 12.dp)
                            Text(statusSentence(agent), Modifier.testTag("agent-status"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        }
                    }
                } else Text("Agent")
            },
            // With the drawer, the menu opens it (the system's Back goes home); at a width where it is always open, Back goes home.
            navigationIcon = {
                if (onMenu != null) IconButton(onClick = onMenu, Modifier.testTag("open-drawer")) { Icon(Icons.Default.Menu, "Menu") }
                else IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") }
            },
            actions = {
                if (agent != null) {
                    IconButton(onClick = { terminal = !terminal }, Modifier.testTag("terminal-toggle").semantics { contentDescription = if (terminal) "Show the chat" else "Show the terminal" }) {
                        Text(">_", fontFamily = FontFamily.Monospace, color = if (terminal) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.titleMedium)
                    }
                    IconButton(onClick = { details = true }, Modifier.testTag("details-open")) { Icon(Icons.Default.MoreVert, "Details") }
                }
            },
        )
    }) { pad ->
        if (agent == null) {
            Text("This agent isn't on the host (any more).", Modifier.padding(pad).padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
            return@Scaffold
        }
        val row = remember(state.runs, state.agents, agentId) { leadRowOf(SidebarHost(runtime.hostId, "", true, false, state.runs, state.agents, emptySet()), agentId) }
        Column(Modifier.inScaffold(pad).fillMaxSize()) {
            // The model the agent runs on (a tap changes it) and a switch under way.
            Row(Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                ModelChip(runtime, agent, canChange = canAct, modifier = Modifier.widthIn(max = 260.dp))
                PendingSwitch(agent, Modifier.weight(1f))
            }
            TreeStrip(row, agent, onOpen = { onAgent(it.id) }, onHelpers = { details = true })
            AnimatedContent(
                terminal, Modifier.weight(1f),
                transitionSpec = { if (reduced) ContentTransform(EnterTransition.None, ExitTransition.None) else fadeIn(tween(160)) togetherWith fadeOut(tween(100)) },
                label = "chat-or-terminal",
            ) { showTerminal ->
                if (showTerminal) TerminalTab(runtime, agentId, Modifier.fillMaxSize())
                else ChatTab(runtime, agentId, Modifier.fillMaxSize(), openTerminal = { terminal = true }, draftState = draft, actionScope = actionScope)
            }
        }
        if (details) DetailsSheet(
            runtime, agent, run, row, state.clockSkewMs, canAct,
            onOpen = { details = false; onAgent(it.id) }, onSend = { send = true }, onDismiss = { details = false },
        )
        if (send) SendMessageSheet(runtime, agentId) { send = false }
    }
}
