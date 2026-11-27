package dev.agentmesh.android.ui.chat

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.chat.Block
import dev.agentmesh.android.chat.ChatSync
import dev.agentmesh.android.chat.POLL_MS
import dev.agentmesh.android.chat.SETTLE_MS
import dev.agentmesh.android.chat.ToolCall
import dev.agentmesh.android.chat.indexOfBlock
import dev.agentmesh.android.chat.say
import dev.agentmesh.android.chat.toBlocks
import dev.agentmesh.android.chat.toolsSummary
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.ui.MonoSmall
import dev.agentmesh.android.ui.collectAsStateWithLifecycleCompat
import dev.agentmesh.android.ui.messages.MarkdownText
import dev.agentmesh.gen.agentmesh.v1.ActorClass
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentState
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch

/** True for a device the host lets read but not act (it refuses a prompt, a switch or a hold). */
fun isReadOnlyDevice(s: HostState): Boolean = s.daemon?.actor?.class_ == ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE

/** Why the composer is off, or empty when it is on. */
fun promptRefusal(agent: Agent, readOnly: Boolean): String = when {
    readOnly -> "This device is read-only: it can read the conversation, not type into the agent."
    agent.state == AgentState.AGENT_STATE_EXITED || agent.state == AgentState.AGENT_STATE_RETIRED || agent.terminalId.isEmpty() ->
        "The agent has stopped, so there is no terminal to type into. Message it from the Messages tab to bring it back."
    else -> ""
}

/**
 * The chat view of an agent: its conversation as readable blocks (what was asked, the reply as Markdown, tool calls
 * folded) and a composer that types into the same running process, as typing in its terminal does. The terminal is the
 * other view of the same agent. Reads only while it is on screen.
 */
@Composable
fun ChatTab(runtime: HostRuntime, agentId: String, modifier: Modifier = Modifier, openTerminal: () -> Unit = {}) {
    val scope = rememberCoroutineScope()
    val sync = remember(runtime, agentId) { ChatSync(scope, runtime::api, agentId) }
    DisposableEffect(sync) { onDispose { sync.stop() } }
    val host by runtime.state.collectAsStateWithLifecycleCompat()
    val agent = host.agents[agentId] ?: return
    val chat by sync.state.collectAsStateWithLifecycleCompat()
    val connected = host.conn is ConnState.Ready

    // Another conversation (a reopen, /clear): what was held is not it.
    LaunchedEffect(sync, agent.sessionId) { sync.reset() }
    LaunchedEffect(sync, agent.sessionId, connected) { if (connected) sync.sync() }
    // Read on when the agent's events say it did something (a burst is one read), and every couple of seconds while it
    // works: a reply fires no event of its own.
    // Keyed on what an event changes about the conversation, not the whole agent: an activity frame changes the agent a few
    // times a second, and each change would start the pause again.
    LaunchedEffect(sync, agent.state, agent.needsInput, agent.phase, agent.terminalId, agent.model, agent.effort) {
        if (connected && chat.loaded) {
            delay(SETTLE_MS)
            sync.sync()
        }
    }
    val working = agent.state == AgentState.AGENT_STATE_WORKING
    LaunchedEffect(sync, working, connected) {
        while (working && connected) {
            delay(POLL_MS)
            sync.sync()
        }
    }

    val blocks = remember(chat.transcript.entries) { toBlocks(chat.transcript.entries) }
    val list = rememberLazyListState()
    // Follow the end while the reader is at it; keep the place when earlier entries come in above (items are keyed).
    var stick by remember { mutableStateOf(true) }
    LaunchedEffect(list) {
        androidx.compose.runtime.snapshotFlow { list.isScrollInProgress to !list.canScrollForward }.collect { (scrolling, atEnd) ->
            if (scrolling || atEnd) stick = atEnd
        }
    }
    // The end follows new entries (the last block changed, or its tool calls grew), not earlier ones coming in above: the
    // header is item 0, so the last block is item `blocks.size`.
    val last = blocks.lastOrNull()
    LaunchedEffect(last?.id, (last as? Block.Tools)?.calls?.size) {
        if (stick && blocks.isNotEmpty()) list.scrollToItem(blocks.size)
    }
    // "Load earlier" keeps the place: the block that was first on screen stays where it was, whatever came in above it.
    var anchor by remember { mutableStateOf<Pair<String, Int>?>(null) }
    LaunchedEffect(blocks.firstOrNull()?.id) {
        val a = anchor ?: return@LaunchedEffect
        val i = indexOfBlock(blocks, a.first)
        if (i >= 0) list.scrollToItem(i + 1, a.second)
        anchor = null
    }
    fun loadEarlier() {
        anchor = list.layoutInfo.visibleItemsInfo.firstOrNull { it.key != "head" }?.let { (it.key as String) to -it.offset }
        stick = false
        sync.loadEarlier()
    }

    Column(modifier.fillMaxSize().imePadding()) {
        LazyColumn(Modifier.weight(1f).fillMaxWidth().testTag("chat"), state = list, contentPadding = androidx.compose.foundation.layout.PaddingValues(12.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            item(key = "head") {
                Column(Modifier.fillMaxWidth(), horizontalAlignment = Alignment.CenterHorizontally) {
                    if (!chat.transcript.supported) {
                        Text(
                            "The chat view reads Claude Code's record of a conversation; ${agent.kind} has none we read yet. Its terminal shows the whole of it, and what you send here is typed into it.",
                            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.testTag("chat-unsupported"),
                        )
                    } else if (!chat.loaded && chat.error.isEmpty()) {
                        Text("Loading the conversation…", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(vertical = 24.dp))
                    }
                    if (chat.error.isNotEmpty()) {
                        Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.testTag("chat-error")) {
                            Text("Couldn't read the conversation: ${chat.error}", Modifier.weight(1f), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                            TextButton(onClick = { sync.sync() }) { Text("Try again") }
                        }
                    }
                    if (chat.loaded && chat.transcript.supported && !chat.transcript.atStart) {
                        TextButton(onClick = ::loadEarlier, enabled = !chat.earlierBusy, modifier = Modifier.testTag("load-earlier")) { Text(if (chat.earlierBusy) "Loading…" else "Load earlier") }
                    }
                    if (chat.loaded && chat.transcript.supported && blocks.isEmpty()) {
                        Text("Nothing said yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(vertical = 24.dp))
                    }
                }
            }
            items(blocks, key = { it.id }) { ChatBlock(it) }
        }
        Text(
            if (working && agent.stateDetail.isNotEmpty()) "Working: ${agent.stateDetail}" else statusSentence(agent),
            Modifier.fillMaxWidth().padding(horizontal = 12.dp).testTag("chat-status"),
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis,
        )
        PromptComposer(runtime, agent, isReadOnlyDevice(host), connected, onSent = sync::afterSend, openTerminal = openTerminal)
    }
}

@Composable
private fun PromptComposer(runtime: HostRuntime, agent: Agent, readOnly: Boolean, connected: Boolean, onSent: () -> Unit, openTerminal: () -> Unit) {
    var text by rememberSaveable(agent.id) { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf("") }
    val scope = rememberCoroutineScope()
    val why = promptRefusal(agent, readOnly)
    val atPrompt = agent.state == AgentState.AGENT_STATE_NEEDS_INPUT
    fun send() {
        val body = text.trim()
        if (body.isEmpty() || busy) return
        busy = true
        error = ""
        scope.launch {
            try {
                runtime.connectedApi().promptAgent(agent.id, body)
                text = ""
                onSent()
            } catch (e: kotlinx.coroutines.CancellationException) {
                throw e
            } catch (e: Exception) {
                // The host's own words: the input box holds a draft, someone is typing.
                error = say(e)
            } finally {
                busy = false
            }
        }
    }
    Column(Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 8.dp)) {
        if (atPrompt && why.isEmpty()) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("It is waiting at a prompt in its terminal: answer it there.", Modifier.weight(1f), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.tertiary)
                OutlinedButton(onClick = openTerminal) { Text("Open the terminal") }
            }
        }
        Row(verticalAlignment = Alignment.Bottom) {
            OutlinedTextField(
                text, { text = it }, Modifier.weight(1f).testTag("prompt"), enabled = why.isEmpty() && !atPrompt,
                placeholder = { Text(if (why.isEmpty()) "Write to the agent" else "") }, maxLines = 6,
            )
            IconButton(onClick = ::send, enabled = why.isEmpty() && !atPrompt && connected && !busy && text.isNotBlank(), modifier = Modifier.testTag("send")) {
                Icon(Icons.AutoMirrored.Filled.Send, "Send")
            }
        }
        // Offline the text stays and Send waits: nothing is held to be sent by a later connection.
        if (why.isEmpty() && !connected) Text("Not connected to the host: reconnect to send.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.testTag("prompt-offline"))
        if (why.isNotEmpty()) Text(why, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.testTag("prompt-why"))
        if (error.isNotEmpty()) Text(error, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error, modifier = Modifier.testTag("prompt-error"))
    }
}

@Composable
private fun ChatBlock(block: Block) {
    when (block) {
        is Block.User -> Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.CenterEnd) {
            Surface(shape = RoundedCornerShape(12.dp), color = MaterialTheme.colorScheme.secondaryContainer, modifier = Modifier.padding(start = 40.dp).testTag("chat-user")) {
                SelectionContainer { Text(block.entry.text, Modifier.padding(horizontal = 12.dp, vertical = 8.dp), style = MaterialTheme.typography.bodyMedium) }
            }
        }
        is Block.Mesh -> Text(
            "From the mesh: ${block.entry.text.replace(Regex("^📬\\s*\\[mesh]\\s*"), "")}",
            Modifier.fillMaxWidth().testTag("chat-mesh"), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        is Block.Assistant -> Column(Modifier.fillMaxWidth().testTag("chat-assistant")) {
            SelectionContainer { MarkdownText(block.entry.text, large = block.entry.text.length > 2_000) }
            if (block.entry.truncated) Text("… cut here", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        is Block.Tools -> ToolsBlock(block)
    }
}

@Composable
private fun ToolsBlock(block: Block.Tools) {
    var open by rememberSaveable(block.id) { mutableStateOf(false) }
    Surface(shape = RoundedCornerShape(8.dp), color = MaterialTheme.colorScheme.surfaceContainer, modifier = Modifier.fillMaxWidth().testTag("chat-tools")) {
        Column {
            Row(Modifier.fillMaxWidth().clickable { open = !open }.heightIn(min = 48.dp).padding(horizontal = 12.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(if (open) "▾" else "▸", Modifier.padding(end = 8.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                Text(toolsSummary(block.calls), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            if (open) Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) { block.calls.forEach { ToolCallRow(it) } }
        }
    }
}

@Composable
private fun ToolCallRow(call: ToolCall) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(call.tool, style = MonoSmall, color = MaterialTheme.colorScheme.onSurface)
            Text(call.input, style = MonoSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        val r = call.result
        if (r == null) Text("Running…", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        else if (r.text.isNotEmpty()) {
            Box(Modifier.fillMaxWidth().heightIn(max = 200.dp).background(MaterialTheme.colorScheme.surface, RoundedCornerShape(4.dp)).verticalScroll(rememberScrollState()).padding(8.dp)) {
                Text(r.text + if (r.truncated) "\n… cut here" else "", style = MonoSmall, color = if (r.isError) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface)
            }
        }
    }
}
