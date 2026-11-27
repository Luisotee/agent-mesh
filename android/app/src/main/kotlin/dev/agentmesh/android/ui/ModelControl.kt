package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
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
import dev.agentmesh.android.chat.say
import dev.agentmesh.android.derive.EFFORTS
import dev.agentmesh.android.derive.familiesOf
import dev.agentmesh.android.derive.pendingText
import dev.agentmesh.android.derive.runsOn
import dev.agentmesh.android.derive.tiersOf
import dev.agentmesh.gen.agentmesh.v1.Agent
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch

/**
 * The CLI, model and effort an agent runs on, a reopen under way, and (for a device that may act) a button to change
 * them: a tier, a model of the host's list for its CLI, or an effort alone. The host reopens the same conversation.
 */
@Composable
fun ModelLine(runtime: HostRuntime, agent: Agent, canChange: Boolean, modifier: Modifier = Modifier) {
    var open by androidx.compose.runtime.saveable.rememberSaveable(agent.id) { mutableStateOf(false) }
    val pending = pendingText(agent)
    Row(modifier.fillMaxWidth().padding(horizontal = 16.dp), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text(runsOn(agent).ifEmpty { agent.kind }, Modifier.testTag("runs-on"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (pending.isNotEmpty()) Text(pending, Modifier.testTag("pending-switch"), style = MaterialTheme.typography.bodySmall, color = stateColors.needs)
        }
        if (canChange) TextButton(onClick = { open = true }, modifier = Modifier.testTag("change-model")) { Text("Change") }
    }
    if (open) ChangeModelSheet(runtime, agent) { open = false }
}

private enum class By(val label: String) { Tier("A tier"), Model("A model"), Effort("Effort only") }

@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
private fun ChangeModelSheet(runtime: HostRuntime, agent: Agent, onDismiss: () -> Unit) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val tiers = remember(state.settings.keys, agent.kind) { tiersOf(state.settings.keys, agent.kind) }
    var by by remember { mutableStateOf(By.Tier) }
    var tier by remember { mutableStateOf(if (agent.tier in tiers) agent.tier else tiers.first()) }
    var model by remember { mutableStateOf("") }
    var effort by remember { mutableStateOf("") }
    var families by remember { mutableStateOf(emptyList<String>()) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    // The host's list for this CLI, read when the sheet opens; without it a model is typed.
    LaunchedEffect(agent.kind) {
        try { families = familiesOf(runtime.awaitApi().checkModels(), agent.kind) } catch (e: CancellationException) { throw e } catch (e: Exception) { }
    }
    fun submit() {
        if (busy) return
        busy = true
        error = ""
        scope.launch {
            try {
                val api = runtime.awaitApi()
                when (by) {
                    By.Tier -> api.switchAgent(agent.id, tier = tier)
                    By.Model -> api.switchAgent(agent.id, model = model.trim(), effort = effort)
                    By.Effort -> api.switchAgent(agent.id, effort = effort)
                }
                onDismiss()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                // The host's own words: already on it, at the quota reserve, a free model of a repo that wants none.
                error = say(e)
            } finally {
                busy = false
            }
        }
    }
    val ready = when (by) { By.Tier -> tier.isNotEmpty(); By.Model -> model.isNotBlank(); By.Effort -> effort.isNotEmpty() }
    ModalBottomSheet(onDismissRequest = { if (!busy) onDismiss() }) {
        Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp).padding(bottom = 16.dp).navigationBarsPadding().testTag("model-sheet"), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("Change ${agent.name}'s model", style = MaterialTheme.typography.titleMedium)
            Text("The host reopens the same conversation on the new model: nothing is briefed again. Now: ${runsOn(agent).ifEmpty { agent.kind }}${if (agent.tier.isNotEmpty()) " (tier ${agent.tier})" else ""}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                By.entries.forEach { b -> FilterChip(selected = by == b, onClick = { by = b }, label = { Text(b.label) }, modifier = Modifier.testTag("by-${b.name}")) }
            }
            when (by) {
                By.Tier -> FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    tiers.forEach { t -> FilterChip(selected = tier == t, onClick = { tier = t }, label = { Text(t) }, modifier = Modifier.testTag("tier-$t")) }
                }
                By.Model -> {
                    OutlinedTextField(model, { model = it }, Modifier.fillMaxWidth().testTag("model"), label = { Text("Model") }, singleLine = true, placeholder = { Text("sonnet") },
                        supportingText = { Text(if (families.isNotEmpty()) "The families the host lists for ${agent.kind}, or any model id." else "A model name or family.") })
                    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) { families.forEach { f -> FilterChip(selected = model == f, onClick = { model = f }, label = { Text(f) }) } }
                }
                By.Effort -> {}
            }
            if (by != By.Tier) {
                Text(if (by == By.Model) "Effort (optional: the model's own when none)" else "Effort (same model, its cache kept)", style = MaterialTheme.typography.labelMedium)
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    EFFORTS.forEach { e -> FilterChip(selected = effort == e, onClick = { effort = if (effort == e && by == By.Model) "" else e }, label = { Text(e) }, modifier = Modifier.testTag("effort-$e")) }
                }
            }
            if (error.isNotEmpty()) Text(error, Modifier.testTag("switch-error"), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            Button(onClick = ::submit, enabled = ready && !busy, modifier = Modifier.fillMaxWidth().testTag("switch")) { Text("Switch") }
        }
    }
}
