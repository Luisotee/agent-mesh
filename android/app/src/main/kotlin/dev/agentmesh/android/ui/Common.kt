package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.size
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.net.isTailnetUrl
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.store.HostState
import java.text.DateFormat
import java.util.Date

/** What the connection is doing, in a sentence. */
fun connSentence(c: ConnState, nowMs: Long = System.currentTimeMillis()): String = when (c) {
    ConnState.Connecting -> "Connecting…"
    is ConnState.Ready -> "Connected"
    is ConnState.Reconnecting -> {
        val s = ((c.retryAtMs - nowMs) / 1000).coerceAtLeast(0)
        "Offline: ${c.error}. ${if (s > 0) "Retrying in $s s" else "Retrying"}"
    }
    is ConnState.Stopped -> when (c.reason) {
        ConnState.Reason.Auth -> "The host refused this device's token"
        ConnState.Reason.Incompatible -> "Incompatible: ${c.message}"
        ConnState.Reason.Config -> c.message
    }
}

fun routeLabel(r: Route): String = when (r) {
    is Route.Direct -> if (isTailnetUrl(r.url)) "tailnet ${r.url}" else r.url
    is Route.Relay -> "relay ${r.relayTcp}"
}

fun clock(ms: Long): String = DateFormat.getTimeInstance(DateFormat.MEDIUM).format(Date(ms))

@Composable
fun ConnDot(c: ConnState, modifier: Modifier = Modifier) {
    val colors = stateColors
    val color: Color = when (c) {
        is ConnState.Ready -> colors.done
        ConnState.Connecting -> colors.working
        is ConnState.Reconnecting -> colors.needs
        is ConnState.Stopped -> colors.failed
    }
    androidx.compose.foundation.layout.Box(modifier.size(10.dp).clip(CircleShape).background(color))
}

/** The strip under the app bar while a host isn't connected: what's wrong and what to do. */
@Composable
fun ConnectionBanner(state: HostState, onRetry: () -> Unit, onLog: () -> Unit) {
    val c = state.conn
    val now = rememberNowMs(c is ConnState.Reconnecting)
    if (c is ConnState.Ready && state.synced) return
    Surface(color = MaterialTheme.colorScheme.surfaceContainerHigh, modifier = Modifier.fillMaxWidth()) {
        Row(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            ConnDot(c)
            Text(connSentence(c, now), Modifier.weight(1f), style = MaterialTheme.typography.bodySmall)
            TextButton(onClick = onLog) { Text("Log") }
            if (c !is ConnState.Connecting) Button(onClick = onRetry) { Text("Retry") }
        }
    }
}

@Composable
fun SectionHeader(text: String, modifier: Modifier = Modifier, trailing: @Composable (() -> Unit)? = null) {
    Row(modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(text, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.weight(1f))
        trailing?.invoke()
    }
}

@Composable
fun KeyValue(key: String, value: String, mono: Boolean = false) {
    Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp)) {
        Text(key, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(value.ifEmpty { "—" }, style = if (mono) MonoSmall else MaterialTheme.typography.bodyMedium)
    }
}

/** The time now in ms, ticking every second while [active]: a "retrying in N s" that counts down. */
@Composable
fun rememberNowMs(active: Boolean): Long {
    val now = androidx.compose.runtime.remember { androidx.compose.runtime.mutableLongStateOf(System.currentTimeMillis()) }
    androidx.compose.runtime.LaunchedEffect(active) {
        while (active) { now.longValue = System.currentTimeMillis(); kotlinx.coroutines.delay(1_000) }
    }
    return now.longValue
}
