package dev.agentmesh.android.ui

import android.content.Context
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.listSaver
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.derive.LeadRow
import dev.agentmesh.android.derive.ProjectRow
import dev.agentmesh.android.derive.QuotaLine
import dev.agentmesh.android.derive.QuotaTone
import dev.agentmesh.android.derive.ShellRow
import dev.agentmesh.android.derive.SidebarHost
import dev.agentmesh.android.derive.Waiting
import dev.agentmesh.android.derive.WaitingKind
import dev.agentmesh.android.derive.WorkerRow
import dev.agentmesh.android.derive.anyWorking
import dev.agentmesh.android.derive.buildProjects
import androidx.lifecycle.repeatOnLifecycle
import dev.agentmesh.android.derive.helpersLabel
import dev.agentmesh.android.derive.hueOf
import dev.agentmesh.android.derive.quotaLines
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.searchProjects
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.derive.tone
import dev.agentmesh.android.derive.usageView
import dev.agentmesh.android.derive.visibleProjects
import dev.agentmesh.android.derive.waitingFor
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.ui.chat.isReadOnlyDevice
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.flowOf
import kotlinx.coroutines.flow.flowOn
import kotlinx.coroutines.Dispatchers

/** A host as the drawer shows it: [note] says why it is down, empty while it is up. */
data class HostStatus(val hostId: String, val name: String, val up: Boolean, val loading: Boolean, val readOnly: Boolean, val note: String)

/** One thing waiting on the user, on the host whose [runtime] it is. */
data class WaitingItem(val runtime: HostRuntime, val w: Waiting)

/** Everything the drawer and the home screen draw, across hosts: built from each host's state, equal when nothing a row shows changed. */
data class SidebarModel(val hosts: List<HostStatus>, val projects: List<ProjectRow>, val waiting: List<WaitingItem>) {
    /** More than one host is paired: rows carry a host badge. */
    val many: Boolean get() = hosts.size > 1
    /** Hosts are paired but none has said anything yet. */
    val loading: Boolean get() = hosts.isNotEmpty() && hosts.all { it.loading }
    val runs: List<LeadRow> get() = projects.flatMap { it.runs }.distinctBy { it.hostId to it.run.id }.sortedWith(compareByDescending<LeadRow> { it.needs > 0 }.thenByDescending { it.last })
}

private fun noteOf(c: ConnState): String = when (c) {
    is ConnState.Reconnecting -> "Offline: ${c.error}"
    is ConnState.Stopped -> connSentence(c)
    else -> ""
}

/** The model of every paired host, kept current while the screen is started. */
@Composable
fun rememberSidebar(): SidebarModel {
    val container = LocalContainer.current
    val hosts by container.hosts.hosts.collectAsStateWithLifecycleCompat()
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    val pins by container.prefs.pins.collectAsStateWithLifecycleCompat()
    val flow = remember(hosts, runtimes, pins) {
        val rts = hosts.mapNotNull { runtimes[it.id] }
        // Hosts paired but their runtimes not built yet: still loading, not "no hosts".
        val loading = SidebarModel(hosts.map { HostStatus(it.id, it.name, up = false, loading = true, readOnly = false, note = "") }, emptyList(), emptyList())
        if (rts.isEmpty()) flowOf(loading)
        else combine(rts.map { it.state }) { states ->
            val side = ArrayList<SidebarHost>()
            val status = ArrayList<HostStatus>()
            val waiting = ArrayList<WaitingItem>()
            rts.forEachIndexed { i, rt ->
                val s = states[i]
                val w = waitingFor(s)
                val name = rt.host?.name ?: rt.hostId
                val up = s.conn is ConnState.Ready
                side.add(SidebarHost(rt.hostId, name, up, !s.synced && s.conn !is ConnState.Stopped && s.conn !is ConnState.Reconnecting, s.runs, s.agents, w.mapTo(HashSet()) { it.agent.id }, s.shells.orEmpty().map { ShellRow(it.id, it.repo, if (it.hasStartedAt()) it.startedAt.seconds * 1000 else 0L, it.exited) }))
                status.add(HostStatus(rt.hostId, name, up, !s.synced && s.conn !is ConnState.Stopped && s.conn !is ConnState.Reconnecting, isReadOnlyDevice(s), noteOf(s.conn)))
                w.forEach { waiting.add(WaitingItem(rt, it)) }
            }
            SidebarModel(status, buildProjects(side, pins), waiting)
        }.flowOn(Dispatchers.Default).distinctUntilChanged()
    }
    // Until the first model is built, the paired hosts are loading (not "no hosts").
    return flow.collectAsStateWithLifecycleCompat(SidebarModel(hosts.map { HostStatus(it.id, it.name, up = false, loading = true, readOnly = false, note = "") }, emptyList(), emptyList())).value
}

/** What the drawer can do: each is a navigation or a sheet the app's root owns. */
class SidebarActions(
    val openAgent: (hostId: String, agentId: String, tab: Tab) -> Unit,
    val home: () -> Unit,
    val settings: () -> Unit,
    val live: () -> Unit,
    val usage: (hostId: String) -> Unit,
    val newLead: (hostId: String, repo: String) -> Unit,
    val addAgent: (hostId: String, runId: String) -> Unit,
    val addProject: (hostId: String) -> Unit,
    /** A project's shell: open it, or start one in a folder (ending one is asked in a dialog, like stopping an agent). */
    val openShell: (hostId: String, shellId: String) -> Unit = { _, _ -> },
    val newShell: (hostId: String, repo: String) -> Unit = { _, _ -> },
)

private val stringSet = listSaver<Set<String>, String>(save = { it.toList() }, restore = { it.toSet() })

/** The drawer's content: search, what waits on the user, every project with its leads and their workers, and the quota. */
/** [active]: the drawer is in view (open, or always open); the quota is read only then. */
@Composable
fun SidebarContent(model: SidebarModel, current: Pair<String, String>?, a: SidebarActions, modifier: Modifier = Modifier, active: Boolean = true) {
    val container = LocalContainer.current
    val hidden by container.prefs.hidden.collectAsStateWithLifecycleCompat()
    var query by rememberSaveable { mutableStateOf("") }
    // The projects whose default fold the user flipped; a search opens everything it finds.
    var flipped by rememberSaveable(stateSaver = stringSet) { mutableStateOf(emptySet<String>()) }
    val searching = query.isNotBlank()
    val found = remember(model.projects, query) { searchProjects(model.projects, query) }
    val (shown, hiddenRows) = remember(found, hidden) { if (searching) found to emptyList() else visibleProjects(found, hidden) }
    var showHidden by rememberSaveable { mutableStateOf(false) }
    // An action that ends or renames something waits here for the user's yes.
    var pending by remember { mutableStateOf<Pair<String, AgentAction>?>(null) }
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    pending?.let { (hostId, action) ->
        val rt = runtimes[hostId]
        if (rt == null) pending = null else AgentActionDialog(rt, action) { _ -> pending = null }
    }
    androidx.compose.runtime.CompositionLocalProvider(LocalAgentAction provides { host, action -> pending = host to action }) {
    Column(modifier.fillMaxHeight().testTag("sidebar")) {
        Row(Modifier.padding(start = 20.dp, end = 8.dp, top = 12.dp), verticalAlignment = Alignment.CenterVertically) {
            Text("agent-mesh", Modifier.weight(1f).clickable(onClick = a.home).testTag("sidebar-title"), style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
        }
        OutlinedTextField(
            query, { query = it }, Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 8.dp).testTag("sidebar-search"), singleLine = true,
            placeholder = { Text("Search runs and agents") }, leadingIcon = { Icon(Icons.Default.Search, null) },
            trailingIcon = { if (searching) IconButton(onClick = { query = "" }) { Icon(Icons.Default.Close, "Clear the search") } },
            shape = RoundedCornerShape(24.dp),
        )
        LazyColumn(Modifier.weight(1f)) {
            model.hosts.filter { !it.up && it.note.isNotEmpty() }.forEach { h ->
                item(key = "off:${h.hostId}") {
                    Text("${h.name}: ${h.note}", Modifier.padding(horizontal = 20.dp, vertical = 4.dp).testTag("host-offline"), style = MaterialTheme.typography.labelMedium, color = stateColors.needs, maxLines = 1, overflow = TextOverflow.Ellipsis)
                }
            }
            if (model.waiting.isNotEmpty() && !searching) {
                item(key = "waiting-header") { SectionHeader("Waiting on you (${model.waiting.size})", Modifier.testTag("sidebar-waiting")) }
                items(model.waiting.size, key = { i -> model.waiting[i].let { "wait:${it.runtime.hostId}:${it.w.kind}:${it.w.message?.id ?: it.w.agent.id}" } }) { i ->
                    val w = model.waiting[i]
                    WaitingRow(w.runtime, w.w, canAct = model.hosts.none { it.hostId == w.runtime.hostId && it.readOnly }) {
                        a.openAgent(w.runtime.hostId, w.w.agent.id, if (w.w.kind == WaitingKind.Input) Tab.Terminal else Tab.Messages)
                    }
                }
                item(key = "waiting-divider") { HorizontalDivider(Modifier.padding(vertical = 8.dp)) }
            }
            if (model.loading) item(key = "loading") { SkeletonRows("Loading the projects", Modifier.padding(horizontal = 16.dp), rows = 4) }
            else if (shown.isEmpty() && hiddenRows.isEmpty() && model.hosts.isNotEmpty()) item(key = "none") {
                Text(if (searching) "Nothing matches." else "No projects yet. Start a lead with +.", Modifier.padding(20.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            shown.forEach { p -> projectItems(p, model, current, a, searching, flipped, { k -> flipped = if (k in flipped) flipped - k else flipped + k }) }
            if (hiddenRows.isNotEmpty()) {
                item(key = "hidden") {
                    TextButton(onClick = { showHidden = !showHidden }, Modifier.padding(horizontal = 8.dp).testTag("hidden-fold")) { Text("${if (showHidden) "▾" else "▸"} Hidden projects (${hiddenRows.size})") }
                }
                if (showHidden) items(hiddenRows.size, key = { "hid:${hiddenRows[it].key}" }) { i ->
                    val p = hiddenRows[i]
                    Row(Modifier.fillMaxWidth().padding(start = 20.dp, end = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                        Text(p.name, Modifier.weight(1f), color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        TextButton(onClick = { container.prefs.unhide(p.key) }) { Text("Show") }
                    }
                }
            }
        }
        QuotaFooter(model, active, a.usage)
        HorizontalDivider()
        Row(Modifier.padding(horizontal = 8.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            IconButton(onClick = a.settings, Modifier.testTag("open-settings")) { Icon(Icons.Default.Settings, "Settings") }
            TextButton(onClick = a.live, Modifier.testTag("open-dock")) { Text("Live") }
            Box(Modifier.weight(1f))
            var menu by remember { mutableStateOf(false) }
            Box {
                IconButton(onClick = { menu = true }, Modifier.testTag("sidebar-new")) { Icon(Icons.Default.Add, "New") }
                DropdownMenu(menu, { menu = false }) {
                    val writable = model.hosts.filter { !it.readOnly }
                    writable.forEach { h ->
                        DropdownMenuItem(text = { Text(if (model.many) "New lead on ${h.name}" else "New lead") }, onClick = { menu = false; a.newLead(h.hostId, "") })
                    }
                    writable.forEach { h ->
                        DropdownMenuItem(text = { Text(if (model.many) "Add a project on ${h.name}…" else "Add a project…") }, onClick = { menu = false; a.addProject(h.hostId) })
                    }
                    if (writable.isEmpty()) DropdownMenuItem(text = { Text(if (model.hosts.isEmpty()) "Pair a host in Settings first" else "This device is read-only") }, enabled = false, onClick = {})
                }
            }
        }
    }
    }
}

private fun androidx.compose.foundation.lazy.LazyListScope.projectItems(
    p: ProjectRow, model: SidebarModel, current: Pair<String, String>?, a: SidebarActions,
    searching: Boolean, flipped: Set<String>, flip: (String) -> Unit,
) {
    // Open by default while something in it lives; a search shows what it found.
    val open = searching || ((p.runs.isNotEmpty() || p.shells.isNotEmpty()) != (p.key in flipped))
    item(key = "p:${p.key}", contentType = "project") { ProjectHeader(p, model, open, a) { flip(p.key) } }
    if (!open) return
    p.shells.forEachIndexed { i, sh -> item(key = "s:${p.key}:${sh.id}", contentType = "shell") { ShellItem(p.hostId, sh, shellLabel(sh, p.shells.size > 1, i), current, a, canAct = model.hosts.none { it.hostId == p.hostId && it.readOnly }) } }
    p.runs.forEach { r -> leadItems(r, p, current, a, model) }
    if (p.also.isNotEmpty()) foldItems("Also working here", "also:${p.key}", p.also, p, current, a, model, searching, flipped, flip)
    if (p.earlier.isNotEmpty()) foldItems("Earlier", "earlier:${p.key}", p.earlier, p, current, a, model, searching, flipped, flip)
    if (p.empty) item(key = "empty:${p.key}") { Text("No runs yet", Modifier.padding(start = 64.dp, bottom = 8.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
}

private fun androidx.compose.foundation.lazy.LazyListScope.foldItems(
    title: String, key: String, rows: List<LeadRow>, p: ProjectRow, current: Pair<String, String>?, a: SidebarActions, model: SidebarModel,
    searching: Boolean, flipped: Set<String>, flip: (String) -> Unit,
) {
    // Closed unless flipped (or searching).
    val open = searching || key in flipped
    item(key = "fold:$key", contentType = "fold") {
        Row(Modifier.fillMaxWidth().clickable { flip(key) }.padding(start = 64.dp, end = 16.dp, top = 6.dp, bottom = 6.dp).testTag("fold-$title"), verticalAlignment = Alignment.CenterVertically) {
            Text("${if (open) "▾" else "▸"} $title (${rows.size})", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
    if (open) rows.forEach { r -> leadItems(r, p, current, a, model, prefix = key) }
}

private fun androidx.compose.foundation.lazy.LazyListScope.leadItems(r: LeadRow, p: ProjectRow, current: Pair<String, String>?, a: SidebarActions, model: SidebarModel, prefix: String = "") {
    item(key = "l:$prefix:${p.key}:${r.run.id}", contentType = "lead") { LeadItem(r, current, a, canAct = model.hosts.none { it.hostId == r.hostId && it.readOnly }) }
    flatWorkers(r.workers, 0).forEach { (w, depth) ->
        item(key = "w:$prefix:${p.key}:${w.agent.id}", contentType = "worker") { WorkerItem(r.hostId, w, depth, current, a, canAct = model.hosts.none { it.hostId == r.hostId && it.readOnly }) }
    }
}

private fun flatWorkers(rows: List<WorkerRow>, depth: Int): List<Pair<WorkerRow, Int>> = rows.flatMap { listOf(it to depth) + flatWorkers(it.workers, depth + 1) }

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun ProjectHeader(p: ProjectRow, model: SidebarModel, open: Boolean, a: SidebarActions, onToggle: () -> Unit) {
    val container = LocalContainer.current
    val context = LocalContext.current
    var menu by remember { mutableStateOf(false) }
    val hidden by container.prefs.hidden.collectAsStateWithLifecycleCompat()
    val hue = LocalMeshTheme.current.hues[hueOf(p.repo) - 1]
    val live = p.runs.size
    val readOnly = model.hosts.any { it.hostId == p.hostId && it.readOnly }
    Row(
        Modifier.fillMaxWidth().combinedClickable(onClick = onToggle, onLongClick = { menu = true }).padding(start = 16.dp, end = 4.dp, top = 6.dp, bottom = 6.dp)
            .testTag("project-${p.name}"),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.size(32.dp).clip(RoundedCornerShape(8.dp)).background(hue.copy(alpha = if (p.up) 0.22f else 0.1f)), contentAlignment = Alignment.Center) {
            Text(p.name.take(1).uppercase(), color = hue, fontWeight = FontWeight.Bold, style = MaterialTheme.typography.labelLarge)
        }
        Column(Modifier.padding(start = 12.dp).weight(1f)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                Text(p.name, style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false), color = if (p.up) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant)
                if (p.pinned) Text("pinned", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                if (model.many) Chip(p.hostName)
            }
            Text(
                buildString {
                    append(if (live == 0) "No live runs" else if (live == 1) "1 live run" else "$live live runs")
                    if (p.shells.isNotEmpty()) append(if (p.shells.size == 1) ", 1 terminal" else ", ${p.shells.size} terminals")
                    if (p.needs > 0) append(", ${p.needs} need you")
                },
                style = MaterialTheme.typography.bodySmall, color = if (p.needs > 0) stateColors.needs else MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis,
            )
        }
        Text(if (open) "▾" else "▸", color = MaterialTheme.colorScheme.onSurfaceVariant)
        // A terminal in this folder: one tap, as the desktop's project row has.
        if (!readOnly && p.repo.isNotEmpty() && p.up) IconButton(onClick = { a.newShell(p.hostId, p.repo) }, Modifier.testTag("new-shell-${p.name}")) {
            Text(">_", fontFamily = androidx.compose.ui.text.font.FontFamily.Monospace, color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.titleSmall)
        }
        Box {
            IconButton(onClick = { menu = true }) { Icon(Icons.Default.MoreVert, "Menu of ${p.name}") }
            DropdownMenu(menu, { menu = false }) {
                DropdownMenuItem(text = { Text("New lead here") }, enabled = !readOnly && p.repo.isNotEmpty(), onClick = { menu = false; a.newLead(p.hostId, p.repo) })
                DropdownMenuItem(text = { Text("New terminal here") }, enabled = !readOnly && p.repo.isNotEmpty() && p.up, onClick = { menu = false; a.newShell(p.hostId, p.repo) })
                DropdownMenuItem(text = { Text(if (p.pinned) "Unpin" else "Pin to the top") }, enabled = p.repo.isNotEmpty(), onClick = { menu = false; if (p.pinned) container.prefs.unpin(p.hostId, p.repo) else container.prefs.pin(p.hostId, p.repo) })
                DropdownMenuItem(text = { Text(if (p.key in hidden) "Show again" else "Hide") }, enabled = p.repo.isNotEmpty(), onClick = { menu = false; if (p.key in hidden) container.prefs.unhide(p.key) else container.prefs.hide(p.key) })
                DropdownMenuItem(text = { Text("Copy the path") }, enabled = p.repo.isNotEmpty(), onClick = { menu = false; copy(context, p.repo) })
            }
        }
    }
}

private fun copy(context: Context, text: String) = copyToClipboard(context, text, "path")

@Composable
private fun Chip(text: String, color: Color = MaterialTheme.colorScheme.onSurfaceVariant) {
    Surface(shape = CircleShape, color = MaterialTheme.colorScheme.surfaceContainerHighest) {
        Text(text, Modifier.padding(horizontal = 8.dp, vertical = 1.dp), style = MaterialTheme.typography.labelSmall, color = color, maxLines = 1)
    }
}

/** A project's shell in the drawer: a terminal in its folder on the host, which keeps running when the app is closed. A long press offers to end it. */
@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun ShellItem(hostId: String, sh: ShellRow, label: String, current: Pair<String, String>?, a: SidebarActions, canAct: Boolean) {
    var menu by remember { mutableStateOf(false) }
    val act = LocalAgentAction.current
    val selected = current == hostId to shellScreenId(sh.id)
    Box {
        Row(
            Modifier.fillMaxWidth().background(if (selected) MaterialTheme.colorScheme.secondaryContainer.copy(alpha = 0.5f) else Color.Transparent)
                .combinedClickable(onClick = { a.openShell(hostId, sh.id) }, onLongClick = { if (canAct) menu = true })
                .padding(start = 64.dp, end = 8.dp, top = 8.dp, bottom = 8.dp).testTag("shell-${sh.id}"),
            verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Text(">_", fontFamily = androidx.compose.ui.text.font.FontFamily.Monospace, color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.titleSmall)
            Text(label, Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium, maxLines = 1)
            if (canAct) IconButton(onClick = { menu = true }, Modifier.size(32.dp)) { Icon(Icons.Default.MoreVert, "Menu of the terminal", Modifier.size(18.dp)) }
        }
        DropdownMenu(menu, { menu = false }) {
            DropdownMenuItem(text = { Text("End the terminal…") }, onClick = { menu = false; act(hostId, AgentAction.EndShell(sh.id, repoName(sh.repo))) }, modifier = Modifier.testTag("shell-end-${sh.id}"))
        }
    }
}

/** "Terminal", or with several in a project "Terminal · 14:02" (when it started, which does not change as others end). */
private fun shellLabel(sh: ShellRow, several: Boolean, index: Int): String =
    if (!several) "Terminal" else if (sh.startedMs > 0) "Terminal · " + java.text.DateFormat.getTimeInstance(java.text.DateFormat.MEDIUM).format(java.util.Date(sh.startedMs)) else "Terminal ${index + 1}"

/** What the drawer's `current` says for a shell's screen (an agent's id is the other kind). */
fun shellScreenId(shellId: String) = "shell:$shellId"

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun LeadItem(r: LeadRow, current: Pair<String, String>?, a: SidebarActions, canAct: Boolean) {
    val lead = r.lead
    val here = lead != null && current == (r.hostId to lead.id)
    var menu by remember { mutableStateOf(false) }
    val canAdd = canAct && r.run.status == RunStatus.RUN_STATUS_OPEN
    val act = LocalAgentAction.current
    Box {
        Row(
            Modifier.fillMaxWidth().then(if (here) Modifier.background(MaterialTheme.colorScheme.secondaryContainer) else Modifier)
                .combinedClickable(enabled = lead != null || canAdd, onClick = { lead?.let { a.openAgent(r.hostId, it.id, Tab.Chat) } ?: run { menu = true } }, onLongClick = { menu = true })
                .padding(start = 28.dp, end = 16.dp, top = 6.dp, bottom = 6.dp).testTag("lead-${r.run.id}"),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            if (lead != null) StateIcon(lead.state, lead.exitCode, lead.stateDetail, statusSentence(lead), size = 18.dp) else Box(Modifier.size(18.dp))
            Column(Modifier.padding(start = 12.dp).weight(1f)) {
                Text(r.title, style = MaterialTheme.typography.bodyLarge, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        lead?.let(::statusSentence) ?: "No agent yet", style = MaterialTheme.typography.bodySmall, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false),
                        color = if (r.needs > 0) stateColors.needs else MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    HelperChip(r.helpers)
                }
            }
            if (lead != null) VendorIcon(lead.kind, lead.model, size = 16.dp)
        }
        // Held down, a run offers what a run can do, and its lead what an agent can. A read-only device sees only what reads.
        DropdownMenu(menu, { menu = false }) {
            if (canAct) {
                DropdownMenuItem(text = { Text("Add an agent") }, enabled = canAdd, onClick = { menu = false; a.addAgent(r.hostId, r.run.id) }, modifier = Modifier.testTag("menu-add-agent"))
                DropdownMenuItem(text = { Text("Rename the run…") }, onClick = { menu = false; act(r.hostId, AgentAction.RenameRun(r.run.id, r.run.title.ifEmpty { r.title })) }, modifier = Modifier.testTag("menu-rename-run"))
                DropdownMenuItem(text = { Text("Stop the run…") }, enabled = r.run.status == RunStatus.RUN_STATUS_OPEN, onClick = { menu = false; act(r.hostId, AgentAction.StopRun(r.run.id, r.title)) }, modifier = Modifier.testTag("menu-stop-run"))
            }
            if (lead != null) AgentMenuItems(r.hostId, lead, canAct, a) { menu = false }
        }
    }
}

/** What an agent's row offers when held down: open it, open its terminal, stop or retire it (each asks first), copy its worktree path. */
@Composable
private fun AgentMenuItems(hostId: String, agent: Agent, canAct: Boolean, a: SidebarActions, dismiss: () -> Unit) {
    val act = LocalAgentAction.current
    val context = LocalContext.current
    val live = agent.state != AgentState.AGENT_STATE_EXITED && agent.state != AgentState.AGENT_STATE_RETIRED
    DropdownMenuItem(text = { Text("Open") }, onClick = { dismiss(); a.openAgent(hostId, agent.id, Tab.Chat) }, modifier = Modifier.testTag("menu-open"))
    if (canAct) {
        DropdownMenuItem(text = { Text("Open its terminal") }, enabled = live && agent.terminalId.isNotEmpty(), onClick = { dismiss(); a.openAgent(hostId, agent.id, Tab.Terminal) }, modifier = Modifier.testTag("menu-terminal"))
        DropdownMenuItem(text = { Text("Stop ${agent.name}…") }, enabled = live, onClick = { dismiss(); act(hostId, AgentAction.StopAgent(agent.id, agent.name)) }, modifier = Modifier.testTag("menu-stop-agent"))
        DropdownMenuItem(text = { Text("Retire ${agent.name}…") }, enabled = live, onClick = { dismiss(); act(hostId, AgentAction.RetireAgent(agent.id, agent.name)) }, modifier = Modifier.testTag("menu-retire-agent"))
    }
    DropdownMenuItem(text = { Text("Copy the worktree path") }, enabled = agent.worktree.isNotEmpty(), onClick = { dismiss(); copy(context, agent.worktree) }, modifier = Modifier.testTag("menu-copy-worktree"))
}

/** What the drawer asks for when the user picks an action that must be confirmed: the host it is on, and the action. */
private val LocalAgentAction = androidx.compose.runtime.staticCompositionLocalOf<(String, AgentAction) -> Unit> { { _, _ -> } }

@Composable
private fun WorkerItem(hostId: String, w: WorkerRow, depth: Int, current: Pair<String, String>?, a: SidebarActions, canAct: Boolean) {
    val agent: Agent = w.agent
    val here = current == (hostId to agent.id)
    var menu by remember { mutableStateOf(false) }
    Box {
    Row(
        Modifier.fillMaxWidth().then(if (here) Modifier.background(MaterialTheme.colorScheme.secondaryContainer) else Modifier)
            .combinedClickable(onClick = { a.openAgent(hostId, agent.id, Tab.Chat) }, onLongClick = { menu = true })
            .padding(start = 52.dp + 16.dp * depth, end = 16.dp, top = 4.dp, bottom = 4.dp).testTag("worker-${agent.id}"),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        StateIcon(agent.state, agent.exitCode, agent.stateDetail, statusSentence(agent), size = 14.dp)
        Row(Modifier.padding(start = 10.dp).weight(1f), horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
            Text(agent.name, style = MaterialTheme.typography.bodyMedium, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
            if (w.elsewhere) Chip(dev.agentmesh.android.derive.repoName(agent.repo))
            HelperChip(w.helpers)
        }
        VendorIcon(agent.kind, agent.model, size = 14.dp)
    }
    DropdownMenu(menu, { menu = false }) { AgentMenuItems(hostId, agent, canAct, a) { menu = false } }
    }
}

/** "2 reviews, 1 asker": the read-only agents folded into a row, as one chip that takes the working colour while one of them works. */
@Composable
private fun HelperChip(helpers: List<Agent>) {
    val label = helpersLabel(helpers)
    if (label.isEmpty()) return
    Chip("·$label", if (anyWorking(helpers)) stateColors.working else MaterialTheme.colorScheme.onSurfaceVariant)
}

/** One meter per quota line of each host, read every minute while the host is up: green, amber from the reserve, red when used up. A tap opens Usage and cost. */
@Composable
private fun QuotaFooter(model: SidebarModel, active: Boolean, onUsage: (String) -> Unit) {
    val lifecycle = androidx.lifecycle.compose.LocalLifecycleOwner.current.lifecycle
    val container = LocalContainer.current
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    val quotas = remember { mutableStateMapOf<String, List<QuotaLine>>() }
    model.hosts.forEach { h ->
        val rt = runtimes[h.hostId]
        androidx.compose.runtime.key(h.hostId) {
            LaunchedEffect(h.hostId, h.up, rt, active) {
                // A host that is down has no reading to show: a frozen figure would pass for the current one.
                if (!h.up || rt == null) { quotas.remove(h.hostId); return@LaunchedEffect }
                // Out of view the reading goes: a reopened drawer shows nothing until a new one lands, not a stale figure.
                if (!active) { quotas.remove(h.hostId); return@LaunchedEffect }
                // Read while the drawer is in view and the app started, not for a drawer nobody can see.
                lifecycle.repeatOnLifecycle(androidx.lifecycle.Lifecycle.State.STARTED) {
                  while (true) {
                    try { quotas[h.hostId] = quotaLines(usageView(rt.awaitApi().getUsage(refresh = false))) } catch (e: CancellationException) { throw e } catch (e: Exception) { quotas.remove(h.hostId) }
                    delay(60_000)
                  }
                }
            }
        }
    }
    val shown = model.hosts.filter { quotas[it.hostId].orEmpty().isNotEmpty() }
    if (shown.isEmpty()) return
    HorizontalDivider()
    Column(Modifier.padding(vertical = 6.dp).testTag("quota-footer")) {
        shown.forEach { h ->
            val rows = quotas.getValue(h.hostId)
            Column(Modifier.fillMaxWidth().clickable { onUsage(h.hostId) }.padding(horizontal = 20.dp, vertical = 4.dp).testTag("quota-${h.name}"), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                if (model.many) Text(h.name, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                rows.forEach { r ->
                    val color = when (r.tone) { QuotaTone.Ok -> stateColors.done; QuotaTone.Warn -> stateColors.needs; QuotaTone.Over -> stateColors.failed }
                    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        Text(r.label, Modifier.width(112.dp), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        Box(Modifier.weight(1f).height(5.dp).clip(CircleShape).background(MaterialTheme.colorScheme.surfaceContainerHighest)) {
                            Box(Modifier.fillMaxHeight().fillMaxWidth((r.used / 100).coerceIn(0.0, 1.0).toFloat()).background(color))
                        }
                        Text("${Math.round(r.used)}%", Modifier.width(36.dp), style = MaterialTheme.typography.labelSmall, color = if (r.tone == QuotaTone.Ok) MaterialTheme.colorScheme.onSurfaceVariant else color, fontWeight = if (r.tone == QuotaTone.Ok) FontWeight.Normal else FontWeight.SemiBold)
                    }
                }
            }
        }
    }
}
