package dev.agentmesh.android.ui.settings

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
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
import dev.agentmesh.android.ui.SectionHeader
import dev.agentmesh.android.ui.collectAsStateWithLifecycleCompat
import kotlinx.coroutines.launch

/** This device's own preferences. They aren't on a host: a theme here changes only this phone. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DeviceSettingsScreen(onBack: () -> Unit) {
    val themes = LocalContainer.current.themes
    val choice by themes.choice.collectAsStateWithLifecycleCompat()
    Scaffold(topBar = {
        TopAppBar(title = { Text("Settings") }, navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } })
    }) { pad ->
        LazyColumn(Modifier.padding(pad).fillMaxSize()) {
            item { SectionHeader("Notifications") }
            item { NotificationSettings() }
            item { SectionHeader("Theme") }
            item { ThemeRow(ThemeStore.SYSTEM, null, choice == ThemeStore.SYSTEM) { themes.choose(ThemeStore.SYSTEM) } }
            items(themes.available, key = { it.name }) { spec ->
                ThemeRow(spec.name, spec, choice == spec.name) { themes.choose(spec.name) }
            }
        }
    }
}

@Composable
private fun ThemeRow(name: String, spec: dev.agentmesh.android.theme.ThemeSpec?, selected: Boolean, onClick: () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        RadioButton(selected, onClick = onClick)
        Text(if (name == ThemeStore.SYSTEM) "Follow the system (${Themes.DEFAULT} or Alucard)" else name, Modifier.weight(1f).padding(start = 8.dp))
        if (spec != null) {
            val t = Themes.resolve(spec)
            Row {
                listOf(t.scheme.background, t.scheme.primary, t.states.working, t.states.needs, t.states.done).forEach {
                    Box(Modifier.padding(start = 4.dp).size(16.dp).clip(CircleShape).background(it).border(1.dp, MaterialTheme.colorScheme.outline, CircleShape))
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
    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        androidx.compose.foundation.layout.Column(Modifier.weight(1f)) {
            Text("Stay connected in the background")
            Text(
                "Keeps your hosts connected so plans, questions and reports notify you with the app closed. Shows a quiet notification while it does.",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        androidx.compose.material3.Switch(background, { container.prefs.setBackground(it); if (it && container.hosts.hosts.value.isNotEmpty()) dev.agentmesh.android.notify.ConnectionService.start(context) })
    }
    PushSettings()
    if (!allowed) {
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
    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        androidx.compose.foundation.layout.Column(Modifier.weight(1f)) {
            Text("Notifications with the app closed")
            val via = container.push.current()?.let { container.push.label(it) }
            Text(
                when {
                    installed.value.isEmpty() -> "Needs a UnifiedPush distributor: install ntfy (F-Droid or Play) and point it at your own ntfy server or ntfy.sh."
                    on && via != null -> "Through $via. What a push carries is encrypted for this phone."
                    else -> "Your hosts push plans, questions and reports through a UnifiedPush distributor (ntfy), end-to-end encrypted, with no connection kept open."
                },
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
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
        androidx.compose.material3.TextButton(
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
