package dev.agentmesh.android.ui

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.input.KeyboardActionHandler
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.input.InputTransformation
import androidx.compose.foundation.text.input.TextFieldState
import androidx.compose.material3.AssistChip
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalSoftwareKeyboardController
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.TextRange
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.term.ImeBuffer
import dev.agentmesh.android.term.Key
import dev.agentmesh.android.term.KeyEncoder
import dev.agentmesh.android.term.Selection
import dev.agentmesh.android.term.TermClient
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.term.TermState
import dev.agentmesh.android.term.cursorKeysApplication
import dev.agentmesh.android.term.pasteBytes
import dev.agentmesh.android.term.selectedText
import dev.agentmesh.android.ui.chat.isReadOnlyDevice

fun termSentence(s: TermState): String = when (s) {
    TermState.WaitingForHost -> "Waiting for the host…"
    TermState.Attaching -> "Attaching…"
    TermState.Live -> "Live"
    is TermState.Reconnecting -> "Reconnecting (${s.attempt}): ${s.why}"
    TermState.Ended -> "The agent's process ended"
    is TermState.Exited -> if (s.signal != 0) "The agent's process was stopped (signal ${s.signal})" else if (s.exitCode == 0) "The agent's process ended" else "The agent's process exited with code ${s.exitCode}"
    is TermState.Failed -> s.message
}

/**
 * An agent's terminal: the host's screen as the agent's CLI draws it, live, with the history to scroll through, and a
 * long press to select text and copy it. A device that may type has a Keyboard switch: with it on the terminal attaches with
 * the input right, the soft keyboard and a key row (Esc, Tab, Ctrl, arrows, Paste) type into the program, and it still
 * attaches with size 0, so typing never resizes the terminal the desktop uses. Off, or on a read-only device, it only watches.
 * It attaches while this tab shows and lets go when it doesn't.
 */
@Composable
fun TerminalTab(runtime: HostRuntime, agentId: String, modifier: Modifier = Modifier) {
    val scope = rememberCoroutineScope()
    val host by runtime.state.collectAsStateWithLifecycleCompat()
    val canType = !isReadOnlyDevice(host)
    var keyboard by rememberSaveable(agentId) { mutableStateOf(false) }
    val typingOn = canType && keyboard
    val screen = remember(agentId) { TermScreen() }
    // Another client when the keyboard flips: the input right is asked for with the ticket, so the stream is opened again.
    val client = remember(agentId, typingOn) {
        TermClient(scope, agentId, { runtime.awaitApi() }, screen, hostReady = { runtime.state.value.conn is ConnState.Ready }, input = typingOn)
    }
    DisposableEffect(client) {
        client.start()
        onDispose { client.stop() }
    }
    val state by client.state.collectAsStateWithLifecycleCompat()
    val typing by client.typing.collectAsStateWithLifecycleCompat()
    // A selection names cells of one screen: a repaint (the desktop resized the terminal, the keyboard flipped) makes another.
    var selection by remember(screen) { mutableStateOf<Selection?>(null) }
    LaunchedEffect(screen.resets) { selection = null }
    // The sticky modifiers are for the keyboard that is open: not carried over a Hide.
    var ctrl by remember(typingOn) { mutableStateOf(false) }
    var alt by remember(typingOn) { mutableStateOf(false) }
    // Counts the taps that ask for the soft keyboard again (it can be dismissed with Back while the switch is on).
    var showKeyboard by remember { mutableIntStateOf(0) }
    val context = LocalContext.current

    fun send(bytes: ByteArray) {
        if (client.type(bytes)) { ctrl = false; alt = false }
    }

    Column(modifier.fillMaxSize().imePadding()) {
        Row(Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            if (canType) FilterChip(selected = keyboard, onClick = { keyboard = !keyboard }, label = { Text("Keyboard") }, modifier = Modifier.testTag("keyboard-toggle"))
            else AssistChip(onClick = {}, label = { Text("Read only") }, modifier = Modifier.testTag("read-only"))
            Text(
                when {
                    typingOn && state == TermState.Live && !typing -> "The host didn't let this device type"
                    typingOn && typing -> "Live · typing"
                    else -> termSentence(state)
                },
                Modifier.weight(1f).testTag("terminal-state"), style = MaterialTheme.typography.bodySmall,
                color = if (state is TermState.Failed) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
            )
            selection?.let { sel ->
                TextButton(onClick = { copy(context, screen.selectedText(sel)); selection = null }, modifier = Modifier.testTag("copy")) { Text("Copy") }
            }
        }
        Box(Modifier.weight(1f).fillMaxWidth()) {
            TerminalCanvas(screen, LocalMeshTheme.current.terminal, selection = selection, onSelection = { selection = it }, onTap = { if (typingOn) showKeyboard++ })
            if (state is TermState.Failed) {
                Button(onClick = client::retry, modifier = Modifier.align(Alignment.Center).testTag("terminal-retry")) { Text("Try again") }
            }
            if (screen.scrollBack > 0) {
                Button(onClick = { screen.scrollBack = 0 }, modifier = Modifier.align(Alignment.BottomEnd).padding(12.dp)) { Text("Latest") }
            }
        }
        if (typingOn) {
            ImeCapture(
                showKeyboard = showKeyboard,
                onBackspace = { send(KeyEncoder.key(Key.Backspace, ctrl = ctrl, alt = alt)) },
                onEnter = { send(KeyEncoder.key(Key.Enter, ctrl = ctrl, alt = alt)) },
                ime = { old, new -> ImeBuffer.change(old, new, ctrl, alt).first.also { if (it.isNotEmpty()) send(it) } },
            )
            TerminalKeyRow(
                ctrl = ctrl, alt = alt, onCtrl = { ctrl = it }, onAlt = { alt = it }, enabled = typing,
                onKey = { k -> send(KeyEncoder.key(k, ctrl, alt, screen.cursorKeysApplication)) },
                onChar = { c -> send(KeyEncoder.text(c, ctrl, alt)) },
                onPaste = { paste(context)?.let { send(screen.pasteBytes(it)) } },
                onHide = { keyboard = false },
            )
        }
    }
}

/**
 * A text field the user can't see, to take the soft keyboard's input: it holds one invisible character, and each edit is
 * handed to [ime] (the bytes go out there) and then reverted, at once and inside the edit, so the next edit, even one that
 * comes before the next frame, is again a change from that character: a typed letter is text after it and Backspace is the
 * character deleted. Enter is the keyboard's Send action or a hardware Enter; no suggestions or composition (the keyboard
 * is told it is a password field). [showKeyboard] counts the taps that ask for the soft keyboard again.
 */
@Composable
private fun ImeCapture(showKeyboard: Int, onBackspace: () -> Unit, onEnter: () -> Unit, ime: (String, String) -> ByteArray) {
    val focus = remember { FocusRequester() }
    val keyboardController = LocalSoftwareKeyboardController.current
    LaunchedEffect(showKeyboard) {
        focus.requestFocus()
        keyboardController?.show()
    }
    val state = remember { TextFieldState(ImeBuffer.SENTINEL, TextRange(ImeBuffer.SENTINEL.length)) }
    val handle by rememberUpdatedState(ime)
    val capture = remember {
        InputTransformation {
            handle(originalText.toString(), asCharSequence().toString())
            revertAllChanges()
        }
    }
    BasicTextField(
        state = state,
        inputTransformation = capture,
        modifier = Modifier.size(1.dp).focusRequester(focus).testTag("ime-capture").onPreviewKeyEvent { e ->
            if (e.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
            when (e.key) {
                androidx.compose.ui.input.key.Key.Enter -> { onEnter(); true }
                androidx.compose.ui.input.key.Key.Backspace -> { onBackspace(); true }
                else -> false
            }
        },
        keyboardOptions = KeyboardOptions(autoCorrectEnabled = false, keyboardType = KeyboardType.Password, imeAction = ImeAction.Send),
        onKeyboardAction = KeyboardActionHandler { onEnter() },
    )
}

private fun copy(context: Context, text: String) {
    if (text.isEmpty()) return
    (context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager).setPrimaryClip(ClipData.newPlainText("terminal", text))
}

private fun paste(context: Context): String? =
    (context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager).primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)?.coerceToText(context)?.toString()?.takeIf { it.isNotEmpty() }
