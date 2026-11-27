package dev.agentmesh.android.ui.chat

import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.gestures.scrollBy
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.text.input.TextFieldState
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.OutlinedTextFieldDefaults
import androidx.compose.runtime.produceState
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.input.KeyboardType
import dev.agentmesh.android.chat.Entry
import dev.agentmesh.android.chat.ago
import dev.agentmesh.android.chat.entryTime
import dev.agentmesh.android.chat.indexOfEntry
import dev.agentmesh.android.chat.mergeTimeline
import dev.agentmesh.android.chat.quoteReply
import dev.agentmesh.android.chat.stamped
import dev.agentmesh.android.derive.messagesOf
import dev.agentmesh.android.derive.waitingFor
import dev.agentmesh.android.ui.copyToClipboard
import dev.agentmesh.android.ui.messages.MessageCard
import dev.agentmesh.android.ui.LocalReducedMotion
import androidx.compose.animation.shrinkVertically
import androidx.compose.animation.expandVertically
import androidx.compose.animation.fadeOut
import androidx.compose.animation.fadeIn
import androidx.compose.animation.ExitTransition
import androidx.compose.animation.EnterTransition
import dev.agentmesh.android.ui.motionItem
import dev.agentmesh.android.ui.SkeletonLines
import dev.agentmesh.android.ui.Skeleton
import dev.agentmesh.android.ui.LocalMeshTheme
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import dev.agentmesh.android.derive.hueOf
import androidx.compose.animation.AnimatedVisibility
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
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.alpha
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.widthIn
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Edit
import dev.agentmesh.gen.agentmesh.v1.QueuedPrompt
import androidx.compose.ui.text.TextRange
import androidx.compose.foundation.text.input.clearText
import androidx.compose.foundation.text.input.rememberTextFieldState
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.MutableState
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
import dev.agentmesh.android.ui.messages.MarkdownCache
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
        "The agent has stopped, so there is no terminal to type into. Send it a message from its details (⋯) to bring it back."
    else -> ""
}

/**
 * The chat view of an agent: its conversation as readable blocks (what was asked, the reply as Markdown, tool calls
 * folded) with the mesh's messages to and from it as cards in their place by time, and a composer that types into the
 * same running process, as typing in its terminal does. Reads only while it is on screen.
 */
@Composable
fun ChatTab(
    runtime: HostRuntime, agentId: String, modifier: Modifier = Modifier, openTerminal: () -> Unit = {},
    draftState: TextFieldState = rememberTextFieldState(),
    /** Where a send and a take-back run: the screen's, which outlives this view (the terminal toggle leaves it), so a refusal can still put the text back. */
    actionScope: kotlinx.coroutines.CoroutineScope? = null,
) {
    val ownScope = rememberCoroutineScope()
    val scope = actionScope ?: ownScope
    val sync = remember(runtime, agentId) { ChatSync(ownScope, runtime::api, agentId, warm = { MarkdownCache.warm(it) }) }
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
    // The mesh's messages stand in the chat at their time, once any are held or the run's history has loaded: the CLI's own
    // doorbell lines stay until then (and for a closed run, whose messages are not loaded), and a reconnect, which keeps what
    // is held, does not flash them back.
    val runId = agent.runId
    val messages = remember(host.messages, agentId) { messagesOf(host.messages, agentId) }
    val messagesFailed = runId in host.messagesFailed
    val waitingOn = remember(host.waiting, host.runs, host.agents, host.messages, agentId) {
        waitingFor(host).filter { it.agent.id == agentId }.mapNotNull { it.message?.id }.toSet()
    }
    // Cards stand in for the doorbell lines only once the run's history has loaded, and stay so over a reconnect (which
    // keeps what is held): a live message before the load must not hide the older lines, nor a failed load all of them.
    val loaded = runId in host.messagesLoaded
    var loadedOnce by remember(runId) { mutableStateOf(false) }
    LaunchedEffect(loaded) { if (loaded) loadedOnce = true }
    val known = loaded || loadedOnce
    val whole = chat.transcript.atStart || !chat.transcript.supported
    val entries = remember(blocks, messages, known, whole, waitingOn) { mergeTimeline(blocks, if (known) messages else emptyList(), cards = known, whole = whole, keep = { it.id in waitingOn }) }
    val stamps = remember(entries) { stamped(entries) }
    val readOnly = isReadOnlyDevice(host)
    val now by produceState(System.currentTimeMillis()) { while (true) { delay(60_000); value = System.currentTimeMillis() } }
    // The user's own words are tinted with the project's identity hue (the same colour on the desktop).
    val s = MaterialTheme.colorScheme
    val hue = LocalMeshTheme.current.hues[hueOf(agent.repo) - 1]
    val bubble = remember(s, hue) { androidx.compose.ui.graphics.lerp(s.surfaceContainerHigh, hue, 0.22f) }
    val list = rememberLazyListState()
    // Follow the end while the reader is at it; keep the place when earlier entries come in above (items are keyed).
    var stick by remember { mutableStateOf(true) }
    LaunchedEffect(list) {
        androidx.compose.runtime.snapshotFlow { list.isScrollInProgress to !list.canScrollForward }.collect { (scrolling, atEnd) ->
            if (scrolling || atEnd) stick = atEnd
        }
    }
    // The end follows new entries (the last one changed, or its tool calls grew), not earlier ones coming in above: the
    // header is item 0, so the last entry is item `entries.size`.
    val last = entries.lastOrNull()
    val queued = host.prompts[agentId].orEmpty()
    // What a take-back said (the host refused, or the prompt was already typed): here, not in the queue, which may be gone by then.
    var queueNote by remember { mutableStateOf("") }
    // The queue stands under the conversation as one more item: the end is that, when there is a queue.
    val tail = entries.size + if (queued.isNotEmpty()) 1 else 0
    val lastChat = entries.lastOrNull { it is Entry.Chat }
    // A tool block above a trailing card grows too: follow that as well.
    val lastTools = (lastChat as? Entry.Chat)?.block as? Block.Tools
    LaunchedEffect(last?.id, ((last as? Entry.Chat)?.block as? Block.Tools)?.calls?.size, lastTools?.calls?.size, queued.size) {
        if (stick && tail > 0) list.scrollToItem(tail)
    }
    // A card or a block that grows after it was placed (Markdown parsed off the main thread, an answer form) keeps the end in view.
    LaunchedEffect(list) {
        // Only the last entry's own growth counts: a list scrolled elsewhere is left where it is.
        androidx.compose.runtime.snapshotFlow { list.layoutInfo.visibleItemsInfo.lastOrNull()?.takeIf { it.index == list.layoutInfo.totalItemsCount - 1 }?.size }.collect { size ->
            if (size != null && stick && !list.isScrollInProgress && list.layoutInfo.totalItemsCount > 1) list.scrollBy(1_000_000f)
        }
    }
    // "Load earlier" keeps the place: the entry that was first on screen stays where it was, whatever came in above it.
    var anchor by remember { mutableStateOf<Pair<String, Int>?>(null) }
    LaunchedEffect(entries.firstOrNull()?.id, blocks.firstOrNull()?.id) {
        val a = anchor ?: return@LaunchedEffect
        val i = indexOfEntry(entries, a.first)
        if (i >= 0) list.scrollToItem(i + 1, a.second)
        anchor = null
    }
    // A load that added nothing above (or failed) leaves no stale place behind.
    LaunchedEffect(chat.earlierBusy) { if (!chat.earlierBusy) anchor = null }
    fun loadEarlier() {
        anchor = list.layoutInfo.visibleItemsInfo.firstOrNull { it.key != "head" }?.let { (it.key as String) to -it.offset }
        stick = false
        sync.loadEarlier()
    }
    // A quoted message opens the reply with its lines marked, above what was already typed.
    // Offered only where the composer takes it (a read-only device or a stopped agent has no field to show it in).
    val quote: ((String) -> Unit)? = if (promptRefusal(agent, readOnly).isEmpty()) ({ text -> draftState.edit { val q = quoteReply(text); replace(0, 0, q); selection = TextRange(q.length) } }) else null
    // What waits to be typed: listed when the chat is shown and again when the agent's state changes (what it waits for
    // follows it) or after a snapshot, kept by the host's events in between.
    LaunchedEffect(runtime, agentId, agent.state, host.snapshots, connected) { if (connected) runtime.refreshPrompts(agentId) }

    Column(modifier.fillMaxSize().imePadding()) {
        LazyColumn(Modifier.weight(1f).fillMaxWidth().testTag("chat"), state = list, contentPadding = PaddingValues(horizontal = 14.dp, vertical = 12.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
            item(key = "head") {
                Column(Modifier.fillMaxWidth(), horizontalAlignment = Alignment.CenterHorizontally) {
                    if (!chat.transcript.supported) {
                        Text(
                            "The chat view reads Claude Code's record of a conversation; ${agent.kind} has none we read yet. Its terminal shows the whole of it, and what you send here is typed into it.",
                            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.testTag("chat-unsupported"),
                        )
                        TextButton(onClick = openTerminal) { Text("Open the terminal") }
                    } else if (!chat.loaded && chat.error.isEmpty()) {
                        SkeletonLines("Loading the conversation", Modifier.fillMaxWidth().padding(vertical = 24.dp).testTag("chat-loading"), lines = 5)
                    }
                    if (chat.error.isNotEmpty()) {
                        Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.testTag("chat-error")) {
                            Text("Couldn't read the conversation: ${chat.error}", Modifier.weight(1f), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                            TextButton(onClick = { sync.sync() }) { Text("Try again") }
                        }
                    }
                    if (messagesFailed) {
                        Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.testTag("messages-failed")) {
                            Text("The mesh's messages didn't load.", Modifier.weight(1f), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                            TextButton(onClick = runtime::reloadMessages, Modifier.testTag("reload-messages")) { Text("Try again") }
                        }
                    }
                    if (chat.loaded && chat.transcript.supported && !chat.transcript.atStart) {
                        TextButton(onClick = ::loadEarlier, enabled = !chat.earlierBusy, modifier = Modifier.testTag("load-earlier")) { Text(if (chat.earlierBusy) "Loading…" else "Load earlier") }
                    }
                    if (chat.loaded && chat.transcript.supported && entries.isEmpty()) {
                        Text("Nothing said yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(vertical = 24.dp))
                    }
                }
            }
            items(entries, key = { it.id }, contentType = { if (it is Entry.Card) "card" else "chat" }) { e ->
                Box(motionItem(glide = false)) {
                    Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                        if (e.id in stamps) Text(ago(entryTime(e) - host.clockSkewMs, now), Modifier.fillMaxWidth().testTag("chat-time"), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
                        when (e) {
                            is Entry.Chat -> ChatBlock(e.block, bubble, quote, running = working && e === lastChat)
                            is Entry.Card -> MessageCard(runtime, e.message, agentId, open = e.message.id in waitingOn, skewMs = host.clockSkewMs, canAnswer = !readOnly)
                        }
                    }
                }
            }
            if (queued.isNotEmpty()) item(key = "queued", contentType = "queued") { QueuedList(runtime, agentId, queued, canTake = !readOnly, canFill = promptRefusal(agent, readOnly).isEmpty(), scope, draftState) { queueNote = it } }
        }
        if (working && agent.stateDetail.isNotEmpty()) {
            Text(
                "Working: ${agent.stateDetail}", Modifier.fillMaxWidth().padding(horizontal = 16.dp).testTag("chat-status"),
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis,
            )
        }
        if (queueNote.isNotEmpty()) Text(queueNote, Modifier.fillMaxWidth().padding(horizontal = 16.dp).testTag("queued-error"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        PromptComposer(runtime, agent, readOnly, connected, draftState, hue, scope, onSent = sync::afterSend, openTerminal = openTerminal)
    }
}

/** What a queued prompt waits for, as the host says (`QueuedPrompt.held`). */
private fun heldText(held: String): String = when (held) {
    "working" -> "Sent when it finishes its turn"
    "starting" -> "Sent once it has started"
    "at_prompt" -> "Sent once the prompt in its terminal is answered"
    "limited" -> "Sent once it is off its usage limit"
    "stopped" -> "Sent when it runs again"
    "draft" -> "Held: its input box holds a draft. Clear it in the terminal, or send this anyway"
    "typing" -> "Held: someone is typing in its terminal"
    "no_box" -> "Held: its screen shows no input box (a dialog may be open in the terminal)"
    else -> "Queued: sent when it is idle"
}

/** Puts a taken-back prompt in the box: never over what was typed meanwhile, which stays above it. */
private fun TextFieldState.fill(text: String) {
    edit {
        val cur = asCharSequence().toString()
        if (cur.isBlank()) { replace(0, length, text); selection = TextRange(text.length) }
        else { val joined = cur.trimEnd() + "\n\n" + text; replace(0, length, joined); selection = TextRange(joined.length) }
    }
}

/** Puts a prompt that was not sent back in the box, ahead of what was typed since. */
private fun TextFieldState.restore(text: String) {
    edit {
        val cur = asCharSequence().toString()
        val joined = if (cur.isBlank()) text else text + "\n\n" + cur.trimStart()
        replace(0, length, joined)
        selection = TextRange(joined.length)
    }
}

/** The prompts that wait to be typed, under the conversation: dimmed bubbles that say what they wait for, each taken back to edit or dropped. */
@Composable
private fun QueuedList(runtime: HostRuntime, agentId: String, prompts: List<QueuedPrompt>, canTake: Boolean, canFill: Boolean, scope: kotlinx.coroutines.CoroutineScope, draft: TextFieldState, note: (String) -> Unit) {
    var busy by remember { mutableStateOf(false) }
    // A prompt the user is being asked to send over the draft in the agent's box.
    var over by remember { mutableStateOf<QueuedPrompt?>(null) }
    fun sendAnyway(p: QueuedPrompt) {
        if (busy) return
        busy = true
        note("")
        scope.launch {
            try {
                runtime.connectedApi().sendPromptAnyway(agentId, p.id, p.heldText)
                runtime.noteWithdrawn(agentId, p.id)
            } catch (e: kotlinx.coroutines.CancellationException) {
                throw e
            } catch (e: Exception) {
                note(say(e)) // the draft changed, someone types, or the agent is not at its prompt: the host says which
            } finally {
                busy = false
            }
        }
    }
    over?.let { p ->
        AlertDialog(
            onDismissRequest = { over = null },
            title = { Text("Send over the draft?") },
            text = { Text("Its input box holds this draft: \u201c${p.heldText}\u201d. Sending clears it and types your prompt instead. The draft is lost.", Modifier.testTag("send-anyway-draft")) },
            confirmButton = { TextButton(onClick = { over = null; sendAnyway(p) }, Modifier.testTag("send-anyway-confirm")) { Text("Send anyway") } },
            dismissButton = { TextButton(onClick = { over = null }) { Text("Cancel") } },
        )
    }
    fun take(p: QueuedPrompt, edit: Boolean) {
        if (busy) return
        busy = true
        note("")
        scope.launch {
            try {
                val got = runtime.connectedApi().withdrawPrompt(agentId, p.id)
                runtime.noteWithdrawn(agentId, p.id)
                // Taken back into the box. A prompt the host typed just before is no longer there to take.
                if (edit && got != null) draft.fill(got.text) else if (edit) note("It was already sent.")
            } catch (e: kotlinx.coroutines.CancellationException) {
                throw e
            } catch (e: Exception) {
                // Typing it had started: it is not withdrawn, and the host says so.
                note(say(e))
            } finally {
                busy = false
            }
        }
    }
    Column(Modifier.fillMaxWidth().testTag("queued"), horizontalAlignment = Alignment.End, verticalArrangement = Arrangement.spacedBy(8.dp)) {
        prompts.forEach { p ->
            Column(Modifier.widthIn(max = 320.dp).testTag("queued-${p.id}"), horizontalAlignment = Alignment.End, verticalArrangement = Arrangement.spacedBy(2.dp)) {
                Text(
                    p.text, Modifier.clip(RoundedCornerShape(18.dp, 18.dp, 4.dp, 18.dp)).border(1.dp, MaterialTheme.colorScheme.outline.copy(alpha = 0.6f), RoundedCornerShape(18.dp, 18.dp, 4.dp, 18.dp))
                        .background(MaterialTheme.colorScheme.surfaceContainerHigh.copy(alpha = 0.5f)).padding(horizontal = 14.dp, vertical = 10.dp).alpha(0.8f),
                    style = MaterialTheme.typography.bodyLarge,
                )
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(heldText(p.held), Modifier.weight(1f, fill = false).testTag("queued-held"), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    if (canTake) {
                        if (p.held == "draft" && p.heldText.isNotEmpty()) TextButton(onClick = { over = p }, Modifier.testTag("queued-anyway-${p.id}")) { Text("Send anyway", style = MaterialTheme.typography.labelSmall) }
                        if (canFill) IconButton(onClick = { take(p, edit = true) }, Modifier.size(32.dp).testTag("queued-edit-${p.id}")) { Icon(Icons.Default.Edit, "Take it back to edit", Modifier.size(16.dp)) }
                        IconButton(onClick = { take(p, edit = false) }, Modifier.size(32.dp).testTag("queued-drop-${p.id}")) { Icon(Icons.Default.Close, "Take it back", Modifier.size(16.dp)) }
                    }
                }
            }
        }
    }
}

@Composable
private fun PromptComposer(
    runtime: HostRuntime, agent: Agent, readOnly: Boolean, connected: Boolean, draft: TextFieldState, hue: Color, scope: kotlinx.coroutines.CoroutineScope,
    onSent: () -> Unit, openTerminal: () -> Unit,
) {
    var busy by remember { mutableStateOf(false) }
    var stopping by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf("") }
    val why = promptRefusal(agent, readOnly)
    val atPrompt = agent.state == AgentState.AGENT_STATE_NEEDS_INPUT
    // Quiet for a while, a working agent shows as unknown: it may still be at work, so Stop stays. Only Claude Code's
    // interrupt key is known to the host (the desktop's Stop shows for it alone as well).
    val working = agent.state == AgentState.AGENT_STATE_WORKING || agent.state == AgentState.AGENT_STATE_UNKNOWN || atPrompt
    val canStop = agent.kind == "claude"
    val blank = draft.text.isBlank()
    // Not at a prompt: Esc there would deny or dismiss the dialog the agent waits on, not stop a turn.
    // Not while a send is in flight: the box is empty then, and a second tap on the same spot must not stop the turn.
    val asStop = working && !atPrompt && canStop && blank && !busy && why.isEmpty()
    fun send() {
        val body = draft.text.toString().trim()
        if (body.isEmpty() || busy) return
        busy = true
        error = ""
        // The box is emptied at the tap, so what is typed while the host answers is a new prompt, and a draft saved when the
        // screen is left is not the one in flight (it would be sent twice). A failure puts the text back.
        draft.clearText()
        scope.launch {
            try {
                // Typed at once when the agent can take it, else queued by the host (it shows under the conversation).
                runtime.prompt(agent.id, body)
                onSent()
            } catch (e: kotlinx.coroutines.CancellationException) {
                throw e
            } catch (e: Exception) {
                draft.restore(body)
                error = say(e)
            } finally {
                busy = false
            }
        }
    }
    fun stop() {
        // One at a time: a second Esc at an agent that already stopped would open Claude's rewind menu.
        if (stopping) return
        stopping = true
        error = ""
        scope.launch {
            try {
                if (!runtime.connectedApi().interruptAgent(agent.id)) error = "Sent Esc, but it hasn't stopped yet: look at its terminal."
            } catch (e: kotlinx.coroutines.CancellationException) {
                throw e
            } catch (e: Exception) {
                error = say(e)
            } finally {
                stopping = false
            }
        }
    }
    Column(Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 8.dp)) {
        if (atPrompt && why.isEmpty()) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("It is waiting at a prompt in its terminal: answer it there. What you send now waits until then.", Modifier.weight(1f).testTag("prompt-at-prompt"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.tertiary)
                OutlinedButton(onClick = openTerminal) { Text("Open the terminal") }
            }
        }
        // The line round the box takes the project's colour while it is focused. A box that is empty while the agent
        // works has Stop where Send is; typing turns it back into Send, which queues.
        MarkdownField(
            draft, tag = "prompt", enabled = why.isEmpty(), accent = hue, shape = RoundedCornerShape(24.dp),
            placeholder = when { why.isNotEmpty() -> ""; working -> "Write to the agent: it is sent when it finishes its turn." + if (canStop && !atPrompt) " The square stops it." else ""; else -> "Write to the agent" },
            trailing = {
                if (asStop) IconButton(onClick = ::stop, enabled = !stopping && connected, modifier = Modifier.testTag("stop")) { Box(Modifier.size(16.dp).background(androidx.compose.material3.LocalContentColor.current, RoundedCornerShape(3.dp)).semantics { contentDescription = "Stop the agent" }) }
                else IconButton(onClick = ::send, enabled = why.isEmpty() && connected && !busy && !blank, modifier = Modifier.testTag("send")) { Icon(Icons.AutoMirrored.Filled.Send, if (working) "Queue" else "Send") }
            },
        )
        // Offline the text stays and Send waits: nothing is held to be sent by a later connection.
        if (why.isEmpty() && !connected) Text("Not connected to the host: reconnect to send.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.testTag("prompt-offline"))
        if (why.isNotEmpty()) Text(why, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.testTag("prompt-why"))
        if (error.isNotEmpty()) Text(error, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error, modifier = Modifier.testTag("prompt-error"))
    }
}

/** What a held-down message offers: copy it, quote it into the reply, or select part of it (a long press would otherwise be taken by the menu). */
@Composable
private fun MessageMenu(open: Boolean, onDismiss: () -> Unit, text: String, quote: ((String) -> Unit)?, onSelect: () -> Unit) {
    val context = LocalContext.current
    DropdownMenu(open, onDismiss) {
        DropdownMenuItem(text = { Text("Copy") }, onClick = { onDismiss(); copyToClipboard(context, text, "message") }, modifier = Modifier.testTag("msg-copy"))
        if (quote != null) DropdownMenuItem(text = { Text("Quote in reply") }, onClick = { onDismiss(); quote(text) }, modifier = Modifier.testTag("msg-quote"))
        DropdownMenuItem(text = { Text("Select text") }, onClick = { onDismiss(); onSelect() }, modifier = Modifier.testTag("msg-select"))
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun ChatBlock(block: Block, bubble: Color, quote: ((String) -> Unit)?, running: Boolean) {
    var menu by remember { mutableStateOf(false) }
    var select by remember { mutableStateOf(false) }
    when (block) {
        // The user's words: a bubble on the right in the project's colour, its corner toward the reader squared off.
        is Block.User -> Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.CenterEnd) {
            Box {
                Surface(
                    shape = RoundedCornerShape(topStart = 18.dp, topEnd = 18.dp, bottomStart = 18.dp, bottomEnd = 4.dp), color = bubble,
                    modifier = Modifier.padding(start = 40.dp).combinedClickable(onClick = {}, onLongClick = { menu = true }).testTag("chat-user"),
                ) {
                    Maybe(select) { Text(block.entry.text, Modifier.padding(horizontal = 14.dp, vertical = 10.dp), style = MaterialTheme.typography.bodyLarge) }
                }
                MessageMenu(menu, { menu = false }, block.entry.text, quote) { select = true }
            }
        }
        // Only when the run's messages are not known yet: a line saying a message came, in place of its card.
        is Block.Mesh -> Text(
            "From the mesh: ${block.entry.text.replace(Regex("^📬\\s*\\[mesh]\\s*"), "")}",
            Modifier.fillMaxWidth().testTag("chat-mesh"), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        // The reply: full-width prose, no bubble and no avatar.
        is Block.Assistant -> Box(Modifier.fillMaxWidth()) {
            Column(Modifier.fillMaxWidth().combinedClickable(onClick = {}, onLongClick = { menu = true }).testTag("chat-assistant")) {
                Maybe(select) { MarkdownText(block.entry.text) }
                if (block.entry.truncated) Text("… cut here", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            MessageMenu(menu, { menu = false }, block.entry.text, quote) { select = true }
        }
        is Block.Tools -> ToolsBlock(block, running)
    }
}

/** [content], selectable word by word once the user asked to select; otherwise plain, so a long press opens the message's menu. */
@Composable
private fun Maybe(select: Boolean, content: @Composable () -> Unit) {
    if (select) SelectionContainer { content() } else content()
}

@Composable
private fun ToolsBlock(block: Block.Tools, running: Boolean) {
    val reduced = LocalReducedMotion.current
    var open by rememberSaveable(block.id) { mutableStateOf(false) }
    // A call that has not answered yet, in the last block while the agent works, is still running.
    val busy = running && block.calls.any { it.result == null }
    Surface(shape = RoundedCornerShape(12.dp), color = MaterialTheme.colorScheme.surfaceContainer, modifier = Modifier.fillMaxWidth().testTag("chat-tools")) {
        Column {
            Row(Modifier.fillMaxWidth().clickable { open = !open }.heightIn(min = 44.dp).padding(horizontal = 12.dp), verticalAlignment = Alignment.CenterVertically) {
                Icon(if (open) Icons.Default.KeyboardArrowUp else Icons.Default.KeyboardArrowDown, if (open) "Fold the tool calls" else "Open the tool calls", Modifier.padding(end = 8.dp).size(18.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                Text(toolsSummary(block.calls), Modifier.weight(1f, fill = false), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                if (busy) Skeleton(Modifier.padding(start = 12.dp).width(40.dp).semantics { contentDescription = "Running" }.testTag("tools-running"), height = 8.dp, radius = 4.dp)
            }
            AnimatedVisibility(open, enter = if (reduced) EnterTransition.None else fadeIn() + expandVertically(), exit = if (reduced) ExitTransition.None else fadeOut() + shrinkVertically()) { Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) { block.calls.forEach { ToolCallRow(it) } } }
        }
    }
}

/** The lines of a tool's answer shown before Show all. */
private const val TOOL_RESULT_LINES = 8

@Composable
private fun ToolCallRow(call: ToolCall) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(call.tool, style = MonoSmall, color = MaterialTheme.colorScheme.onSurface)
            Text(call.input, style = MonoSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        val r = call.result
        if (r == null) Skeleton(Modifier.fillMaxWidth(0.4f).semantics { contentDescription = "Running" }, height = 10.dp)
        else if (r.text.isNotEmpty()) {
            // No scroll of its own inside the chat's list (a drag that began on it would be taken from the list): a few lines, and Show all.
            var all by rememberSaveable(call.id) { mutableStateOf(false) }
            val long = r.text.length > 400 || r.text.count { it == '\n' } >= TOOL_RESULT_LINES
            Column(Modifier.fillMaxWidth().background(MaterialTheme.colorScheme.surface, RoundedCornerShape(4.dp)).padding(8.dp)) {
                Text(
                    r.text + if (r.truncated && all) "\n… cut here" else "", style = MonoSmall, color = if (r.isError) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
                    maxLines = if (all) Int.MAX_VALUE else TOOL_RESULT_LINES, overflow = TextOverflow.Ellipsis,
                )
                if (long) Text(
                    if (all) "Show less" else "Show all", Modifier.clickable { all = !all }.padding(top = 6.dp).testTag("tool-result-toggle"),
                    style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary,
                )
            }
        }
    }
}
