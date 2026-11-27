package dev.agentmesh.android.derive

import dev.agentmesh.android.store.createdMillis
import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.Run
import dev.agentmesh.gen.agentmesh.v1.RunStatus

/**
 * The drawer's projects across every host (the desktop's `sidebar.ts`, from desktop PR #193, with its rules). A project is a repo some run is
 * led from or works in, plus the ones the user pinned. Nothing here reads the network: the host's runs, agents and what
 * waits on the user come in as [SidebarHost].
 */
data class SidebarHost(
    val hostId: String,
    val hostName: String,
    /** Its connection is up: a host that is down greys its projects out. */
    val up: Boolean,
    /** Its first snapshot hasn't arrived on a connection that is coming up. */
    val loading: Boolean,
    val runs: Map<String, Run>,
    val agents: Map<String, Agent>,
    /** The agents that wait on the user. */
    val waiting: Set<String>,
)

/** A worker of a run, with its read-only agents (reviewers, askers) folded into it and the workers it started under it. */
data class WorkerRow(val agent: Agent, val helpers: List<Agent>, val workers: List<WorkerRow>, /** It works in another repo than its lead. */ val elsewhere: Boolean)

/** A run, shown as its lead. */
data class LeadRow(
    val hostId: String,
    val run: Run,
    /** The run's lead; none for a run that has no agent yet. */
    val lead: Agent?,
    val helpers: List<Agent>,
    val workers: List<WorkerRow>,
    /** Agents of the run that wait on the user. */
    val needs: Int,
    /** The run's latest activity (ms). */
    val last: Long,
    /** Closed, or nothing in it is alive: it folds under "Earlier". */
    val earlier: Boolean,
) {
    val title: String get() = run.title.ifEmpty { lead?.name ?: run.id }
}

data class ProjectRow(
    /** `hostId` and repo path: the same folder on two hosts is two projects. */
    val key: String,
    val hostId: String,
    val hostName: String,
    val up: Boolean,
    val repo: String,
    val name: String,
    /** Runs led from this project, live ones. */
    val runs: List<LeadRow>,
    /** Its closed and finished runs. */
    val earlier: List<LeadRow>,
    /** Runs led from another project with a worker here. */
    val also: List<LeadRow>,
    val needs: Int,
    val last: Long,
    val pinned: Boolean,
) {
    val empty: Boolean get() = runs.isEmpty() && earlier.isEmpty() && also.isEmpty()
}

fun projectKey(hostId: String, repo: String) = "$hostId\n$repo"

private fun alive(a: Agent) = a.state != AgentState.AGENT_STATE_EXITED && a.state != AgentState.AGENT_STATE_RETIRED
private fun helper(a: Agent) = a.role == AgentRole.AGENT_ROLE_REVIEWER || a.role == AgentRole.AGENT_ROLE_ASKER
private fun activity(a: Agent) = maxOf(a.createdMillis, if (a.hasLastActivity()) a.lastActivity.toMillis() else 0L)
private fun Run.createdMs() = if (hasCreatedAt()) createdAt.seconds * 1000 else 0L

/** The nodes minus the archived ones, an archived agent's children lifted into its place (they may still run). */
private fun unarchived(nodes: List<TreeNode>): List<TreeNode> = nodes.flatMap { if (it.agent.archived) unarchived(it.children) else listOf(it) }

private class Split(val helpers: List<Agent>, val workers: List<WorkerRow>)

/** A tree node's workers and helpers: read-only agents fold into the agent above them, at any depth under it. */
private fun split(nodes: List<TreeNode>, leadRepo: String): Split {
    val helpers = ArrayList<Agent>()
    val workers = ArrayList<WorkerRow>()
    for (n in unarchived(nodes)) {
        val under = split(n.children, leadRepo)
        if (helper(n.agent)) {
            // Whatever a read-only agent started folds in with it.
            helpers.add(n.agent)
            helpers.addAll(under.helpers)
            helpers.addAll(flat(under.workers))
        } else workers.add(WorkerRow(n.agent, under.helpers, under.workers, n.agent.repo.isNotEmpty() && n.agent.repo != leadRepo))
    }
    return Split(helpers, workers)
}

private fun flat(rows: List<WorkerRow>): List<Agent> = rows.flatMap { listOf(it.agent) + it.helpers + flat(it.workers) }

/** Every agent of a lead row: the lead, its workers and their helpers. */
fun LeadRow.agents(): List<Agent> = listOfNotNull(lead) + helpers + flat(workers)

/** The workers of a lead row, at any depth. */
fun allWorkers(rows: List<WorkerRow>): List<WorkerRow> = rows.flatMap { listOf(it) + allWorkers(it.workers) }

/** The runs of one host as lead rows (archived agents left out, their children lifted; a run whose every agent is archived left out). */
fun leadRows(h: SidebarHost): List<LeadRow> {
    val out = ArrayList<LeadRow>()
    for (t in buildTrees(h.runs, h.agents)) {
        val roots = unarchived(t.roots)
        if (t.roots.isNotEmpty() && roots.isEmpty()) continue
        // The lead is the run's root agent; any other top-level agent (its parent unknown) shows as one of its workers.
        val leadAt = roots.indexOfFirst { it.agent.role == AgentRole.AGENT_ROLE_ROOT }.coerceAtLeast(0)
        val lead = roots.getOrNull(leadAt)
        val rest = roots.filterIndexed { i, _ -> i != leadAt }
        val parts = split(lead?.children.orEmpty() + rest, lead?.agent?.repo.orEmpty())
        val base = LeadRow(h.hostId, t.run, lead?.agent, parts.helpers, parts.workers, 0, 0, false)
        val all = base.agents()
        out.add(
            base.copy(
                needs = all.count { it.state == AgentState.AGENT_STATE_NEEDS_INPUT || it.id in h.waiting },
                last = maxOf(t.run.createdMs(), all.maxOfOrNull(::activity) ?: 0L),
                earlier = t.run.status == RunStatus.RUN_STATUS_CLOSED || (all.isNotEmpty() && all.none(::alive)),
            ),
        )
    }
    return out.sortedWith(compareByDescending<LeadRow> { it.needs > 0 }.thenByDescending { it.last })
}

/** The run [agentId] belongs to as a lead row (its lead, workers and folded helpers), for the agent screen's strip; null when it is in none. */
fun leadRowOf(h: SidebarHost, agentId: String): LeadRow? = leadRows(h).firstOrNull { r -> r.agents().any { it.id == agentId } }

/**
 * The projects of every host: pinned first (in pin order), then those with something waiting on the user, then by latest
 * activity. A run sits under its lead's project; a worker in another repo lists the run under that project's [ProjectRow.also].
 * [pins] are repo paths by host id.
 */
fun buildProjects(hosts: List<SidebarHost>, pins: Map<String, List<String>>): List<ProjectRow> {
    class Acc(val h: SidebarHost, val repo: String, val pinned: Boolean) {
        val runs = ArrayList<LeadRow>()
        val earlier = ArrayList<LeadRow>()
        val also = ArrayList<LeadRow>()
        var needs = 0
        var last = 0L
    }
    val by = LinkedHashMap<String, Acc>()
    fun get(h: SidebarHost, repo: String) = by.getOrPut(projectKey(h.hostId, repo)) { Acc(h, repo, repo in pins[h.hostId].orEmpty()) }
    for (h in hosts) {
        for (repo in pins[h.hostId].orEmpty()) get(h, repo)
        for (r in leadRows(h)) {
            val p = get(h, r.lead?.repo.orEmpty())
            if (r.earlier) p.earlier.add(r) else { p.runs.add(r); p.needs += r.needs }
            p.last = maxOf(p.last, r.last)
            if (r.earlier) continue
            for (repo in allWorkers(r.workers).filter { it.elsewhere }.map { it.agent.repo }.toSet()) {
                val q = get(h, repo)
                q.also.add(r)
                q.last = maxOf(q.last, r.last)
            }
        }
    }
    fun pinAt(p: Acc) = pins[p.h.hostId].orEmpty().indexOf(p.repo).let { if (it < 0) Int.MAX_VALUE else it }
    fun row(key: String, p: Acc) =
        ProjectRow(key, p.h.hostId, p.h.hostName, p.h.up, p.repo, repoName(p.repo).ifEmpty { "No project" }, p.runs, p.earlier, p.also, p.needs, p.last, p.pinned)
    return by.entries
        .sortedWith(
            compareByDescending<Map.Entry<String, Acc>> { it.value.pinned }
                .thenBy { pinAt(it.value) }
                .thenBy { if (it.value.pinned) it.value.h.hostName else "" }
                .thenByDescending { it.value.needs > 0 }
                .thenByDescending { it.value.last }
                .thenBy { repoName(it.value.repo) },
        )
        .map { row(it.key, it.value) }
}

/** "2 reviews", "1 asker", "2 reviews, 1 asker": a worker's read-only agents as one chip, or nothing. */
fun helpersLabel(helpers: List<Agent>): String {
    val reviews = helpers.count { it.role == AgentRole.AGENT_ROLE_REVIEWER }
    val asks = helpers.size - reviews
    return listOfNotNull(
        if (reviews > 0) "$reviews ${if (reviews == 1) "review" else "reviews"}" else null,
        if (asks > 0) "$asks ${if (asks == 1) "asker" else "askers"}" else null,
    ).joinToString(", ")
}

/** Whether any of these agents is working now (the helpers chip spins while one is). */
fun anyWorking(agents: List<Agent>) = agents.any { it.state == AgentState.AGENT_STATE_WORKING || it.state == AgentState.AGENT_STATE_STARTING }

/** Whether a project's row is shown: a hidden one comes back while something in it waits on the user. */
fun visibleProjects(rows: List<ProjectRow>, hidden: Set<String>): Pair<List<ProjectRow>, List<ProjectRow>> =
    rows.partition { it.needs > 0 || it.key !in hidden }

/**
 * What the search field keeps: the projects whose name or path has every word of [query], with all their runs, and else
 * the runs (with the project around them) whose title, lead, workers, CLI or model has every word. An empty query keeps all.
 */
fun searchProjects(rows: List<ProjectRow>, query: String): List<ProjectRow> {
    val words = query.lowercase().split(Regex("\\s+")).filter { it.isNotEmpty() }
    if (words.isEmpty()) return rows
    fun hit(text: String) = words.all { it in text.lowercase() }
    fun hit(r: LeadRow) = hit((listOf(r.title) + r.agents().flatMap { listOf(it.name, it.kind, it.model) }).joinToString(" "))
    return rows.mapNotNull { p ->
        if (hit(p.name + " " + p.repo)) return@mapNotNull p
        val runs = p.runs.filter(::hit)
        val earlier = p.earlier.filter(::hit)
        val also = p.also.filter(::hit)
        if (runs.isEmpty() && earlier.isEmpty() && also.isEmpty()) null else p.copy(runs = runs, earlier = earlier, also = also)
    }
}
