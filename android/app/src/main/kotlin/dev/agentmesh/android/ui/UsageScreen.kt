package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.chat.say
import dev.agentmesh.android.derive.COST_WINDOWS
import dev.agentmesh.android.derive.CostRow
import dev.agentmesh.android.derive.CostView
import dev.agentmesh.android.derive.QuotaBar
import dev.agentmesh.android.derive.UsageView
import dev.agentmesh.android.derive.costView
import dev.agentmesh.android.derive.resetText
import dev.agentmesh.android.derive.usageView
import dev.agentmesh.android.store.ConnState
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

private const val QUOTA_EVERY_MS = 60_000L

/**
 * Quota and spend of the host, the same as `mesh usage` and `mesh cost`: Claude's windows and agy's rows against the
 * reserve line, and the spend (tokens first, dollars as the API-equivalent beside them) of a span or one run. The quotas
 * are read again every minute while the screen is open; the spend when it opens, the span or run changes, or Refresh is
 * pressed. A phone shows it as bars and cards, not the desktop's wide tables.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun UsageScreen(runtime: HostRuntime, onBack: () -> Unit, quotaEveryMs: Long = QUOTA_EVERY_MS) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val connected = state.conn is ConnState.Ready
    var usage by remember { mutableStateOf<UsageView?>(null) }
    var usageError by remember { mutableStateOf("") }
    var cost by remember { mutableStateOf<CostView?>(null) }
    var costError by remember { mutableStateOf("") }
    // "24h", "7d", "30d", or "run:<id>".
    var scope by rememberSaveable { mutableStateOf("7d") }
    var refresh by remember { mutableIntStateOf(0) }

    LaunchedEffect(connected, refresh) {
        if (!connected) return@LaunchedEffect
        // Refresh asks the host to bypass its cache once; the timed reads after it take what the host has.
        var forced = refresh > 0
        while (true) {
            try {
                usage = usageView(runtime.awaitApi().getUsage(refresh = forced))
                forced = false
                usageError = ""
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                usageError = say(e)
            }
            delay(quotaEveryMs)
        }
    }
    LaunchedEffect(connected, scope, refresh) {
        if (!connected) return@LaunchedEffect
        cost = null
        try {
            val api = runtime.awaitApi()
            cost = costView(if (scope.startsWith("run:")) api.getCost(runId = scope.removePrefix("run:")) else api.getCost(since = scope))
            costError = ""
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            costError = say(e)
        }
    }
    val now = rememberNowMs(true)

    Scaffold(topBar = {
        TopAppBar(
            title = { Text("Usage · ${runtime.host?.name ?: "Host"}") },
            navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } },
            actions = { IconButton(onClick = { refresh++ }, modifier = Modifier.testTag("refresh")) { Icon(Icons.Default.Refresh, "Refresh") } },
        )
    }) { pad ->
        Column(Modifier.padding(pad).fillMaxSize().verticalScroll(rememberScrollState())) {
            ConnectionBanner(state, onRetry = runtime::nudge, onLog = {})
            SectionHeader("Quota")
            usage?.let { QuotaSection(it, now) } ?: if (usageError.isEmpty()) Text("Reading the quotas…", Modifier.padding(horizontal = 16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant) else Unit
            if (usageError.isNotEmpty()) Text("Couldn't read the quotas: $usageError", Modifier.padding(horizontal = 16.dp).testTag("usage-error"), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            HorizontalDivider(Modifier.padding(vertical = 12.dp))
            SectionHeader("Spend")
            SpendScope(scope, state.runs.values.sortedByDescending { it.createdAt.seconds }.map { it.id to it.title.ifEmpty { it.id } }) { scope = it }
            cost?.let { CostSection(it) } ?: if (costError.isEmpty()) Text("Reading the spend…", Modifier.padding(horizontal = 16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant) else Unit
            if (costError.isNotEmpty()) Text("Couldn't read the spend: $costError", Modifier.padding(horizontal = 16.dp).testTag("cost-error"), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
        }
    }
}

private val dayAndTime = SimpleDateFormat("EEE HH:mm", Locale.getDefault())

@Composable
private fun QuotaSection(v: UsageView, now: Long) {
    Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("The host starts nothing new on a quota that is ${v.reserve}% used.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        if (v.warnings.isNotEmpty()) Text("At the reserve: ${v.warnings.joinToString(", ")}", Modifier.testTag("quota-warning"), color = stateColors.needs, style = MaterialTheme.typography.bodyMedium)
        if (v.claude.isEmpty()) Text("No Claude quota reading on this host.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        v.claude.forEach { Bar(it, now) }
        v.agy.forEach { r ->
            Column(Modifier.testTag("agy-row")) {
                Row { Text("agy ${r.group} · ${r.label}", Modifier.weight(1f)); Text("${r.left}% left", color = if (r.atReserve) stateColors.needs else MaterialTheme.colorScheme.onSurface) }
                if (r.resets.isNotEmpty()) Text("resets ${r.resets}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
        v.tiers.forEach { t ->
            Text(
                buildString { append("${t.tier}: ${t.model.ifEmpty { "every model is at the reserve" }}"); if (t.left != null) append(" (${t.left}% left)"); if (t.notes.isNotEmpty()) append(" · ${t.notes.joinToString("; ")}") },
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

@Composable
private fun Bar(b: QuotaBar, now: Long) {
    Column(Modifier.testTag("quota-bar")) {
        Row { Text(b.label, Modifier.weight(1f)); Text("${Math.round(b.used)}% used", color = if (b.atReserve) stateColors.needs else MaterialTheme.colorScheme.onSurface) }
        LinearProgressIndicator(progress = { (b.used / 100.0).toFloat().coerceIn(0f, 1f) }, Modifier.fillMaxWidth().padding(vertical = 4.dp), color = if (b.atReserve) stateColors.needs else MaterialTheme.colorScheme.primary)
        Text(resetText(b.resetsAtMs, now) { dayAndTime.format(Date(it)) }, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun SpendScope(scope: String, runs: List<Pair<String, String>>, onChange: (String) -> Unit) {
    var menu by remember { mutableStateOf(false) }
    FlowRow(Modifier.padding(horizontal = 16.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        COST_WINDOWS.forEach { (since, label) -> FilterChip(selected = scope == since, onClick = { onChange(since) }, label = { Text(label) }, modifier = Modifier.testTag("span-$since")) }
        Column {
            FilterChip(selected = scope.startsWith("run:"), onClick = { menu = true }, label = { Text(if (scope.startsWith("run:")) runs.firstOrNull { it.first == scope.removePrefix("run:") }?.second ?: "A run" else "One run") }, modifier = Modifier.testTag("span-run"))
            DropdownMenu(menu, { menu = false }) {
                runs.forEach { (id, title) -> DropdownMenuItem(text = { Text(title) }, onClick = { onChange("run:$id"); menu = false }) }
            }
        }
    }
}

@Composable
private fun CostSection(v: CostView) {
    Column(Modifier.fillMaxWidth().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(v.scope, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(v.tokens, Modifier.testTag("cost-tokens"), style = MaterialTheme.typography.headlineSmall)
        Text(v.breakdown, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text("${v.usd} at API prices${if (v.pricesAsOf.isNotEmpty()) " (as of ${v.pricesAsOf})" else ""}: what the Claude sessions would cost per token, beside the tokens, not the measure.", Modifier.testTag("cost-usd"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        v.bases.forEach { (cli, basis) -> if (basis.isNotEmpty()) Text("$cli counts: $basis", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
        v.sections.forEach { s ->
            SectionHeader(s.title, Modifier.padding(horizontal = 0.dp))
            s.rows.forEach { CostRowCard(it) }
        }
        v.notes.forEach { Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
    }
}

@Composable
private fun CostRowCard(r: CostRow) {
    Column(Modifier.fillMaxWidth().testTag("cost-row"), verticalArrangement = Arrangement.spacedBy(2.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(r.name, Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
            Text("${r.tokens} tokens", style = MaterialTheme.typography.bodyMedium)
        }
        Text("${r.detail} · ${r.usd}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}
