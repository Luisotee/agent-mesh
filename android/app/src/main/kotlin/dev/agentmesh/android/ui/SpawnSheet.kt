package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import dev.agentmesh.android.ui.chat.MarkdownField
import androidx.compose.foundation.text.input.rememberTextFieldState
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
import dev.agentmesh.android.derive.CLI_KINDS
import dev.agentmesh.android.derive.SpawnForm
import dev.agentmesh.android.derive.defaultRole
import dev.agentmesh.android.derive.defaultTier
import dev.agentmesh.android.derive.knownRepos
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.slugOf
import dev.agentmesh.android.derive.spawnProblem
import dev.agentmesh.android.derive.tiersOf
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch

/**
 * Starts something from the phone, with the registry actions the desktop's forms call: a new run (`StartRun`, then its lead
 * with `SpawnAgent`) when [runId] is null, or an agent added to the run [runId] (a lead if it has none, else a worker). The repo is
 * one of the host's projects (`ListProjects`) or a path typed; the tier is one of the host's own for the CLI (`tiers.<cli>.*`).
 * Only a device that may act has it: the host refuses a read-only one, and the screen doesn't offer it.
 */
@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
fun SpawnSheet(runtime: HostRuntime, runId: String?, onDismiss: () -> Unit, onStarted: (agentId: String) -> Unit = {}, repo: String = "") {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    val hasLead = runId != null && state.agents.values.any { it.runId == runId && it.role == AgentRole.AGENT_ROLE_ROOT }
    var title by remember { mutableStateOf("") }
    var form by remember { mutableStateOf(SpawnForm(repo = repo)) }
    val taskBox = rememberTextFieldState()
    // A new run's agent is its lead; an added one is the lead if the run has none, else a worker.
    val role = if (runId == null) AgentRole.AGENT_ROLE_ROOT else defaultRole(hasLead)
    var chosenRole by remember(role) { mutableStateOf(role) }
    val tiers = remember(state.settings.keys, form.kind) { tiersOf(state.settings.keys, form.kind.trim()) }
    var chosenTier by remember { mutableStateOf<String?>(null) }
    val tier = chosenTier?.takeIf { chosenRole == AgentRole.AGENT_ROLE_ROOT || it in tiers } ?: defaultTier(chosenRole, tiers)
    var withAgent by remember { mutableStateOf(true) }
    var projects by remember { mutableStateOf(emptyList<Pair<String, String>>()) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    // The run a first press made: a retry after the agent was refused adds to it, and doesn't open another.
    var startedRun by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()
    val connected = state.conn is ConnState.Ready

    // The host's repos, read when the sheet opens; without them a path is typed.
    LaunchedEffect(Unit) {
        try { projects = runtime.awaitApi().listProjects().map { it.path to it.name.ifEmpty { repoName(it.path) } } } catch (e: CancellationException) { throw e } catch (e: Exception) { }
    }
    val suggestions = remember(projects, state.agents) { (projects.map { it.first } + knownRepos(state.agents.values)).distinct().take(12) }

    val current = form.copy(role = chosenRole, tier = tier, slug = form.slug.ifEmpty { slugOf(title) }, task = taskBox.text.toString())
    val problem = if (runId == null && title.isBlank()) "Give the run a title." else if (runId == null && !withAgent) null else spawnProblem(current)

    fun submit() {
        if (busy) return
        busy = true
        error = ""
        scope.launch {
            try {
                val api = runtime.connectedApi()
                val run = runId ?: startedRun ?: api.startRun(title.trim()).id.also { startedRun = it }
                if (runId != null || withAgent) {
                    val r = api.spawnAgent(run, current.repo.trim(), current.slug.trim(), current.role, current.kind.trim(), current.tier, current.model.trim(), current.task.trim())
                    onStarted(r.agent.id)
                }
                onDismiss()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                // The host's own words: the agent cap, the quota reserve, a repo that isn't one.
                error = say(e)
            } finally {
                busy = false
            }
        }
    }

    ModalBottomSheet(onDismissRequest = { if (!busy) onDismiss() }) {
        Column(
            Modifier.fillMaxWidth().navigationBarsPadding().imePadding().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(bottom = 16.dp).testTag("spawn-sheet"),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Text(if (runId == null) "New lead" else "Add an agent", style = MaterialTheme.typography.titleMedium)
            if (runId == null) {
                OutlinedTextField(title, { title = it }, Modifier.fillMaxWidth().testTag("run-title"), label = { Text("Title") }, singleLine = true, placeholder = { Text("Fix the typo in the README") })
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Checkbox(withAgent, { withAgent = it }, modifier = Modifier.testTag("with-agent"))
                    Text("Start its lead agent now")
                }
            }
            if (runId != null || withAgent) {
                OutlinedTextField(form.repo, { form = form.copy(repo = it) }, Modifier.fillMaxWidth().testTag("repo"), label = { Text("Repository") }, singleLine = true, placeholder = { Text("/home/you/projects/app") },
                    supportingText = { Text("A path on the host, or a folder of its repos directory.") })
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    suggestions.forEach { r -> FilterChip(selected = form.repo == r, onClick = { form = form.copy(repo = r) }, label = { Text(repoName(r)) }, modifier = Modifier.testTag("repo-$r")) }
                }
                OutlinedTextField(current.slug, { form = form.copy(slug = it) }, Modifier.fillMaxWidth().testTag("slug"), label = { Text("Name") }, singleLine = true, placeholder = { Text("fix-typo") },
                    supportingText = { Text("Names the branch and the agent.") })
                if (runId != null) {
                    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        FilterChip(selected = chosenRole == AgentRole.AGENT_ROLE_ROOT, onClick = { chosenRole = AgentRole.AGENT_ROLE_ROOT }, label = { Text("Lead") }, modifier = Modifier.testTag("role-lead"))
                        FilterChip(selected = chosenRole == AgentRole.AGENT_ROLE_WORKER, onClick = { chosenRole = AgentRole.AGENT_ROLE_WORKER }, label = { Text("Worker") }, modifier = Modifier.testTag("role-worker"))
                    }
                }
                Text("CLI", style = MaterialTheme.typography.labelMedium)
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    CLI_KINDS.forEach { k -> FilterChip(selected = form.kind == k, onClick = { form = form.copy(kind = k); chosenTier = null }, label = { Text(k) }, modifier = Modifier.testTag("kind-$k")) }
                }
                Text("Tier (each is a model and effort in the host's settings)", style = MaterialTheme.typography.labelMedium)
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    if (chosenRole == AgentRole.AGENT_ROLE_ROOT) FilterChip(selected = tier.isEmpty(), onClick = { chosenTier = "" }, label = { Text("The CLI's default") }, modifier = Modifier.testTag("tier-default"))
                    tiers.forEach { t -> FilterChip(selected = tier == t, onClick = { chosenTier = t }, label = { Text(t) }, modifier = Modifier.testTag("tier-$t")) }
                }
                OutlinedTextField(form.model, { form = form.copy(model = it) }, Modifier.fillMaxWidth().testTag("model"), label = { Text("Or a model by name (optional)") }, singleLine = true)
                MarkdownField(taskBox, Modifier.fillMaxWidth(), tag = "task", placeholder = "First message (optional)", minLines = 2, shape = androidx.compose.foundation.shape.RoundedCornerShape(12.dp))
            }
            if (!connected) Text("Not connected to the host: reconnect to start.", Modifier.testTag("spawn-offline"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            else if (problem != null) Text(problem, Modifier.testTag("spawn-problem"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (error.isNotEmpty()) Text(error, Modifier.testTag("spawn-error"), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            Button(onClick = ::submit, enabled = connected && problem == null && !busy, modifier = Modifier.fillMaxWidth().testTag("spawn-go")) {
                Text(if (runId == null) (if (withAgent) "Start the run and its lead" else "Start the run") else "Start the agent")
            }
        }
    }
}
