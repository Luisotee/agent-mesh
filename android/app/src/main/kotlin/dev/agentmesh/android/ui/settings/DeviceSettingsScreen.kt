package dev.agentmesh.android.ui.settings

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.ui.semantics.Role
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.theme.ThemeStore
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.notify.DEFAULT_EVENTS
import dev.agentmesh.android.ui.SectionHeader
import dev.agentmesh.android.ui.collectAsStateWithLifecycleCompat
import kotlinx.coroutines.launch

/**
 * This device's own preferences, in groups with a line each on what they are for. They aren't on a host: a theme chosen
 * here overrides the host's `app.theme` on this phone only.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DeviceSettingsScreen(onBack: () -> Unit, onHosts: () -> Unit = {}) {
    val themes = LocalContainer.current.themes
    val hostCount by LocalContainer.current.hosts.hosts.collectAsStateWithLifecycleCompat()
    val choice by themes.choice.collectAsStateWithLifecycleCompat()
    Scaffold(topBar = {
        TopAppBar(title = { Text("Settings") }, navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } })
    }) { pad ->
        Column(Modifier.padding(pad).fillMaxSize().verticalScroll(rememberScrollState()).padding(bottom = 24.dp)) {
            SettingsGroup("Hosts", "The machines this phone works with.") {
                SettingRow(
                    "Paired hosts",
                    if (hostCount.isEmpty()) "None yet: pair with a host to begin." else hostCount.joinToString(", ") { it.name },
                    Modifier.clickable(onClick = onHosts).testTag("settings-hosts"),
                ) { Text("›", color = MaterialTheme.colorScheme.onSurfaceVariant) }
            }
            SettingsGroup("Notifications", "What reaches you, and how, while you are not looking at the app.") { NotificationSettings() }
            SettingsGroup("Appearance", "Colours for this phone only. A theme chosen here wins over the host's app.theme.") {
                ThemeRow(ThemeStore.HOST, choice == ThemeStore.HOST) { themes.choose(ThemeStore.HOST) }
                SettingDivider()
                ThemeRow(ThemeStore.SYSTEM, choice == ThemeStore.SYSTEM) { themes.choose(ThemeStore.SYSTEM) }
                SettingDivider()
                ThemeCards(themes.available.map { it.name to Themes.resolve(it) }, choice) { themes.choose(it) }
            }
        }
    }
}

@Composable
private fun ThemeRow(name: String, selected: Boolean, onClick: () -> Unit) {
    SettingRow(
        if (name == ThemeStore.HOST) "Follow the host" else "Follow the system",
        if (name == ThemeStore.HOST) "Use the theme the host sets (its app.theme setting)." else "${Themes.DEFAULT} in the dark, Alucard in the light.",
        Modifier.selectable(selected, onClick = onClick, role = Role.RadioButton).testTag("theme-$name"),
    ) { RadioButton(selected, onClick = null) }
}

/** The themes as swatch cards drawn from their own files: the page, the accent and the state colours, under the name. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun ThemeCards(themes: List<Pair<String, dev.agentmesh.android.theme.MeshTheme>>, choice: String, onChoose: (String) -> Unit) {
    FlowRow(Modifier.fillMaxWidth().padding(12.dp), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalArrangement = Arrangement.spacedBy(10.dp), maxItemsInEachRow = 2) {
        themes.forEach { (name, t) ->
            val on = choice == name
            Column(
                Modifier.weight(1f).clip(RoundedCornerShape(12.dp)).background(t.scheme.background)
                    .border(if (on) 2.dp else 1.dp, if (on) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant, RoundedCornerShape(12.dp))
                    .selectable(on, onClick = { onChoose(name) }, role = Role.RadioButton).padding(12.dp).testTag("theme-$name"),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                Text(name, style = MaterialTheme.typography.labelLarge, color = t.scheme.onBackground, maxLines = 1)
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    listOf(t.scheme.primary, t.states.working, t.states.needs, t.states.done).forEach { Box(Modifier.size(14.dp).clip(CircleShape).background(it)) }
                }
            }
        }
    }
}

@Composable
private fun NotificationSettings() {
    val container = LocalContainer.current
    val background by container.prefs.background.collectAsStateWithLifecycleCompat()
    val context = androidx.compose.ui.platform.LocalContext.current
    // Read again on returning from the system's settings page.
    val allowedState = androidx.compose.runtime.remember { androidx.compose.runtime.mutableStateOf(container.notifier.allowed()) }
    androidx.lifecycle.compose.LifecycleResumeEffect(Unit) { allowedState.value = container.notifier.allowed(); onPauseOrDispose { } }
    val allowed = allowedState.value
    SettingRow(
        "Stay connected in the background",
        "Keeps your hosts connected so plans, questions and reports notify you with the app closed. Shows a quiet notification while it does.",
    ) {
        androidx.compose.material3.Switch(background, { container.prefs.setBackground(it); if (it && container.hosts.hosts.value.isNotEmpty()) dev.agentmesh.android.notify.ConnectionService.start(context) })
    }
    SettingDivider()
    PushSettings()
    SettingDivider()
    NotifyKindsSettings()
    if (!allowed) {
        SettingDivider()
        Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            Text("Notifications are blocked for this app.", Modifier.weight(1f), color = MaterialTheme.colorScheme.error)
            androidx.compose.material3.TextButton(onClick = {
                context.startActivity(
                    android.content.Intent(android.provider.Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                        .putExtra(android.provider.Settings.EXTRA_APP_PACKAGE, context.packageName).addFlags(android.content.Intent.FLAG_ACTIVITY_NEW_TASK),
                )
            }) { Text("Open settings") }
        }
    }
}

/** What each kind of notice is called on the settings screen, in the order of the host's `app.notify.events`. */
val NOTICE_LABELS = listOf(
    "input" to "An agent stopped at a prompt in its terminal",
    "plan" to "A plan to approve",
    "question" to "A question to answer",
    "report" to "A report (an agent is done)",
    "failed" to "A failure",
    "error" to "An agent whose last turn ended in an error",
    "limited" to "An agent at its usage limit",
    "exited" to "Any agent's clean exit",
)

/**
 * What this phone is told of, for notifications on the phone and for pushes: follow each host's own list (its
 * `app.notify.events`), or choose here, one list for every host. The host's `app.notify.enabled` still silences everything.
 */
@Composable
private fun NotifyKindsSettings() {
    val container = LocalContainer.current
    val chosen by container.prefs.notifyEvents.collectAsStateWithLifecycleCompat()
    val follow = chosen == null
    SettingRow(
        "What to be told of",
        if (follow) "Follows each host's own list (its app.notify.events)." else "One list for this phone, whatever a host's own says; it goes to every host for pushes.",
    ) {
        androidx.compose.material3.Switch(
            !follow, { on -> container.prefs.setNotifyEvents(if (on) DEFAULT_CHOICE else null) },
            modifier = Modifier.testTag("choose-events"),
        )
    }
    if (!follow) {
        val set = chosen.orEmpty()
        Text(
            "At least one kind stays on. To hear nothing, turn notifications off on the host (its app.notify.enabled setting).",
            Modifier.padding(horizontal = 32.dp).testTag("events-note"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        NOTICE_LABELS.forEach { (key, label) ->
            Row(Modifier.fillMaxWidth().padding(horizontal = 32.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(label, Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
                // At least one stays on: none at all is the host's own "notifications off" switch, not a list (said below).
                androidx.compose.material3.Switch(
                    key in set, { on -> container.prefs.setNotifyEvents(if (on) set + key else if (set.size > 1) set - key else set) },
                    enabled = !(key in set && set.size == 1), modifier = Modifier.testTag("event-$key"),
                )
            }
        }
    }
}

/** What turning the choice on starts with: the default list (the daemon's `DefaultNotifyEvents`, which `DEFAULT_EVENTS` mirrors). */
private val DEFAULT_CHOICE = DEFAULT_EVENTS.map { it.key }.toSet()

/**
 * Notifications with the app closed, through UnifiedPush: a switch, what it runs through (or that a distributor
 * such as ntfy is missing), how each host stands, and a test button.
 */
@Composable
private fun PushSettings() {
    val container = LocalContainer.current
    val on by container.prefs.push.collectAsStateWithLifecycleCompat()
    val status by container.pushRegistrar.status.collectAsStateWithLifecycleCompat()
    val hosts by container.hosts.hosts.collectAsStateWithLifecycleCompat()
    val context = androidx.compose.ui.platform.LocalContext.current
    val scope = androidx.compose.runtime.rememberCoroutineScope()
    // Read again on returning to the app (the user may have just installed ntfy).
    val installed = androidx.compose.runtime.remember { androidx.compose.runtime.mutableStateOf(container.push.distributors()) }
    androidx.lifecycle.compose.LifecycleResumeEffect(Unit) { installed.value = container.push.distributors(); onPauseOrDispose { } }
    var note by androidx.compose.runtime.remember { androidx.compose.runtime.mutableStateOf<String?>(null) }
    val via = container.push.current()?.let { container.push.label(it) }
    SettingRow(
        "Notifications with the app closed",
        when {
            installed.value.isEmpty() -> "Needs a UnifiedPush distributor: install ntfy (F-Droid or Play) and point it at your own ntfy server or ntfy.sh."
            on && via != null -> "Through $via. What a push carries is encrypted for this phone."
            else -> "Your hosts push plans, questions and reports through a UnifiedPush distributor (ntfy), end-to-end encrypted, with no connection kept open."
        },
    ) {
        androidx.compose.material3.Switch(
            on,
            { want ->
                note = null
                if (!want) container.disablePush()
                else (context as? android.app.Activity)?.let { a ->
                    container.enablePush(a) { ok -> if (!ok) note = "No distributor was chosen: install ntfy, then try again." }
                }
            },
            enabled = on || installed.value.isNotEmpty(),
            modifier = Modifier.testTag("push-switch"),
        )
    }
    if (on) {
        for (h in hosts) {
            val st = status[h.id]
            Text(
                "${h.name}: ${st?.text ?: "Waiting for the distributor"}",
                Modifier.padding(horizontal = 16.dp, vertical = 2.dp),
                style = MaterialTheme.typography.bodySmall,
                color = if (st?.ok == true) MaterialTheme.colorScheme.onSurfaceVariant else MaterialTheme.colorScheme.error,
            )
        }
        if (hosts.isNotEmpty()) androidx.compose.material3.TextButton(
            onClick = {
                note = "Sending…"
                scope.launch {
                    val errs = hosts.mapNotNull { h -> container.pushRegistrar.test(h.id)?.let { "${h.name}: $it" } }
                    note = if (errs.isEmpty()) "Sent: a test notification should arrive in a moment." else errs.joinToString("\n")
                }
            },
            modifier = Modifier.padding(horizontal = 8.dp),
        ) { Text("Send a test notification") }
    }
    note?.let { Text(it, Modifier.padding(horizontal = 16.dp, vertical = 2.dp), style = MaterialTheme.typography.bodySmall) }
}
