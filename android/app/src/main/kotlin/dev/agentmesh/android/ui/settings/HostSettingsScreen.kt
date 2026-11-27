package dev.agentmesh.android.ui.settings

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Checkbox
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
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
import dev.agentmesh.android.net.toJson
import dev.agentmesh.android.net.toValue
import dev.agentmesh.android.ui.SectionHeader
import dev.agentmesh.android.ui.collectAsStateWithLifecycleCompat
import dev.agentmesh.gen.agentmesh.v1.Setting
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonPrimitive

/** The host's settings, the same from every device: a list generated from the schemas the host sends. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HostSettingsScreen(runtime: HostRuntime, onBack: () -> Unit) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    // A key chord (`app.keys.*`) is for a keyboard, and the host's own device edits it there: a phone has none to press.
    val shown = remember(state.settings) { state.settings.values.filter { !isShortcut(it) } }
    val hidden = state.settings.size - shown.size
    val groups = remember(shown) { shown.sortedBy { it.key }.groupBy { groupOf(it.key) }.toSortedMap() }
    var editing by remember { mutableStateOf<String?>(null) }
    Scaffold(topBar = {
        TopAppBar(title = { Text("${runtime.host?.name ?: "Host"} settings") }, navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } })
    }) { pad ->
        if (groups.isEmpty()) {
            Text("The host's settings load once it is connected.", Modifier.padding(pad).padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
        } else {
            LazyColumn(Modifier.padding(pad).fillMaxSize()) {
                groups.forEach { (group, list) ->
                    item(key = "g:$group") { SectionHeader(group) }
                    items(list, key = { it.key }) { s -> SettingRow(s) { editing = s.key } }
                }
                if (hidden > 0) item(key = "hidden-keys") {
                    Text(
                        "$hidden keyboard shortcuts (the desktop's) are not shown here: a phone has no keyboard to press them on. Set them in the desktop app.",
                        Modifier.padding(16.dp).testTag("hidden-shortcuts"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
    }
    state.settings[editing]?.let { s -> SettingDialog(runtime, s, onClose = { editing = null }) }
}

/** A setting whose value is a key chord (`app.keys.*`). */
fun isShortcut(s: Setting): Boolean = s.schema.type == "shortcut"

/** What a setting is called: the host's label, or its key for a host that sends none. */
fun labelOf(s: Setting): String = s.schema.label.ifEmpty { s.key }

private fun valueText(s: Setting): String {
    val c = controlFor(s.schema.type, s.schema.choicesList)
    val t = toText(c, s.value.toJson()).replace('\n', ' ')
    return if (t.length > 60) t.take(59) + "…" else t
}

@Composable
private fun SettingRow(s: Setting, onClick: () -> Unit) {
    Column(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 10.dp)) {
        Text(labelOf(s), style = MaterialTheme.typography.bodyMedium)
        Text(s.schema.summary, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(
            valueText(s) + if (s.isSet) "" else "  (default)",
            style = MaterialTheme.typography.labelMedium, color = if (s.isSet) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun SettingDialog(runtime: HostRuntime, s: Setting, onClose: () -> Unit) {
    val control = remember(s.key) { controlFor(s.schema.type, s.schema.choicesList) }
    var text by remember(s.key) { mutableStateOf(toText(control, s.value.toJson())) }
    var flag by remember(s.key) { mutableStateOf(s.value.toJson().asBool()) }
    var picked by remember(s.key) { mutableStateOf(s.value.toJson().asStrings().toSet()) }
    var error by remember(s.key) { mutableStateOf<String?>(null) }
    var busy by remember(s.key) { mutableStateOf(false) }
    val scope = rememberCoroutineScope()

    fun send(block: suspend () -> Unit) {
        busy = true; error = null
        scope.launch {
            try { block(); onClose() } catch (e: Exception) { error = e.message ?: e.javaClass.simpleName }
            busy = false
        }
    }

    AlertDialog(
        onDismissRequest = onClose,
        title = { Text(labelOf(s)) },
        text = {
            Column {
                Text(s.schema.summary, style = MaterialTheme.typography.bodySmall)
                if (s.schema.restart) Text("Read when the host starts: a change applies at its next restart.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                when (control) {
                    Control.Bool -> Row(Modifier.padding(top = 12.dp), verticalAlignment = Alignment.CenterVertically) {
                        Switch(flag, { flag = it }); Text(if (flag) "On" else "Off", Modifier.padding(start = 12.dp))
                    }
                    is Control.Select -> {
                        var open by remember { mutableStateOf(false) }
                        OutlinedButton(onClick = { open = true }, Modifier.padding(top = 12.dp)) { Text(text.ifEmpty { "choose" }) }
                        DropdownMenu(open, { open = false }) {
                            control.choices.forEach { c -> DropdownMenuItem(text = { Text(c) }, onClick = { text = c; open = false }) }
                        }
                    }
                    is Control.Multi -> Column(Modifier.padding(top = 8.dp)) {
                        control.choices.forEach { c ->
                            Row(Modifier.fillMaxWidth().clickable { picked = if (c in picked) picked - c else picked + c }, verticalAlignment = Alignment.CenterVertically) {
                                Checkbox(c in picked, { picked = if (it) picked + c else picked - c }); Text(c)
                            }
                        }
                    }
                    else -> OutlinedTextField(
                        text, { text = it }, Modifier.fillMaxWidth().padding(top = 12.dp),
                        singleLine = control is Control.Number || control == Control.Text,
                        minLines = if (control == Control.Lines || control == Control.RawJson) 3 else 1,
                        supportingText = { if (control == Control.Lines) Text("One entry per line") },
                    )
                }
                error?.let { Text(it, color = MaterialTheme.colorScheme.error, modifier = Modifier.padding(top = 8.dp)) }
            }
        },
        confirmButton = {
            TextButton(enabled = !busy, onClick = {
                val value: JsonElement = when (control) {
                    Control.Bool -> JsonPrimitive(flag)
                    is Control.Multi -> kotlinx.serialization.json.JsonArray(control.choices.filter { it in picked }.map(::JsonPrimitive))
                    is Control.Select -> JsonPrimitive(text)
                    else -> when (val p = fromText(s.schema.type, control, text)) {
                        is Parsed.Ok -> p.value
                        is Parsed.Bad -> { error = p.error; return@TextButton }
                    }
                }
                send { runtime.setSetting(s.key, value.toValue()) }
            }) { Text("Save") }
        },
        dismissButton = {
            Row {
                if (s.isSet) TextButton(enabled = !busy, onClick = { send { runtime.resetSetting(s.key) } }) { Text("Reset to default") }
                TextButton(onClick = onClose) { Text("Cancel") }
            }
        },
    )
}
