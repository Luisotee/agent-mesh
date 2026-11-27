package dev.agentmesh.android.ui

import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.defaultMinSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.term.Key

/** What a key of the row sends: a key, or a character. */
sealed interface RowKey {
    data class Special(val key: Key) : RowKey
    data class Char(val text: String) : RowKey

    /** Ctrl and a letter in one tap (`^C`): the interrupt a phone's keyboard has no Ctrl to make. */
    data class Control(val letter: String) : RowKey
}

/** The keys the soft keyboard has none of (or hides): Esc, Tab, arrows, Home, End, paging and the characters a shell needs. */
val KEY_ROW: List<Pair<String, RowKey>> = listOf(
    "Esc" to RowKey.Special(Key.Esc), "Tab" to RowKey.Special(Key.Tab), "⇤Tab" to RowKey.Special(Key.ShiftTab),
    "^C" to RowKey.Control("c"), "^D" to RowKey.Control("d"), "^Z" to RowKey.Control("z"), "^L" to RowKey.Control("l"),
    "↑" to RowKey.Special(Key.Up), "↓" to RowKey.Special(Key.Down), "←" to RowKey.Special(Key.Left), "→" to RowKey.Special(Key.Right),
    "Home" to RowKey.Special(Key.Home), "End" to RowKey.Special(Key.End), "PgUp" to RowKey.Special(Key.PageUp), "PgDn" to RowKey.Special(Key.PageDown),
    "Del" to RowKey.Special(Key.Delete), "Enter" to RowKey.Special(Key.Enter),
    "/" to RowKey.Char("/"), "-" to RowKey.Char("-"), "|" to RowKey.Char("|"), "~" to RowKey.Char("~"), "\\" to RowKey.Char("\\"),
)

/**
 * The row above the soft keyboard: sticky Ctrl and Alt (one key, then off), the keys above, Paste, and Hide. It sends nothing
 * itself: each press is handed to [onKey], [onChar] or [onPaste] with the modifiers that were on, which [onModifiersUsed] turns off.
 */
@Composable
fun TerminalKeyRow(
    ctrl: Boolean,
    alt: Boolean,
    onCtrl: (Boolean) -> Unit,
    onAlt: (Boolean) -> Unit,
    enabled: Boolean,
    onKey: (Key) -> Unit,
    onChar: (String) -> Unit,
    onControl: (String) -> Unit,
    onPaste: () -> Unit,
    onHide: () -> Unit,
    modifier: Modifier = Modifier,
) {
    Surface(color = MaterialTheme.colorScheme.surfaceContainer, modifier = modifier.fillMaxWidth().testTag("key-row")) {
        Row(
            Modifier.horizontalScroll(rememberScrollState()).padding(horizontal = 8.dp, vertical = 4.dp),
            horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically,
        ) {
            FilterChip(selected = ctrl, onClick = { onCtrl(!ctrl) }, label = { Text("Ctrl") }, modifier = Modifier.testTag("key-ctrl"))
            FilterChip(selected = alt, onClick = { onAlt(!alt) }, label = { Text("Alt") }, modifier = Modifier.testTag("key-alt"))
            KEY_ROW.forEach { (label, k) ->
                OutlinedButton(
                    onClick = { when (k) { is RowKey.Special -> onKey(k.key); is RowKey.Char -> onChar(k.text); is RowKey.Control -> onControl(k.letter) } },
                    enabled = enabled, modifier = Modifier.defaultMinSize(minWidth = 48.dp, minHeight = 48.dp).testTag("key-$label"),
                    contentPadding = androidx.compose.foundation.layout.PaddingValues(horizontal = 10.dp),
                ) { Text(label) }
            }
            OutlinedButton(onClick = onPaste, enabled = enabled, modifier = Modifier.defaultMinSize(minHeight = 48.dp).testTag("key-paste")) { Text("Paste") }
            OutlinedButton(onClick = onHide, modifier = Modifier.defaultMinSize(minHeight = 48.dp).testTag("key-hide")) { Text("Hide") }
        }
    }
}
