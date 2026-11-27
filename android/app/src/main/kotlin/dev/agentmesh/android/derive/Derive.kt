package dev.agentmesh.android.derive

import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.store.MessageOrder
import dev.agentmesh.android.store.createdMillis
import dev.agentmesh.android.store.isBefore
import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.Run
import dev.agentmesh.gen.agentmesh.v1.RunStatus

class TreeNode(val agent: Agent, val children: List<TreeNode>)

class RunTree(
    val run: Run,
    val roots: List<TreeNode>,
    /** Agents of the run that need the user: asking for input. */
    val needsInput: Int,
    /** Agents still alive. */
    val live: Int,
)

/** Roots first (the run's lead), then the rest in start order. */
private val AgentOrder = Comparator<Agent> { a, b ->
    val lead = (if (b.role == AgentRole.AGENT_ROLE_ROOT) 1 else 0) - (if (a.role == AgentRole.AGENT_ROLE_ROOT) 1 else 0)
    if (lead != 0) lead else {
        val t = a.createdMillis.compareTo(b.createdMillis)
        if (t != 0) t else a.name.compareTo(b.name)
    }
}

private val Run.createdSeconds: Long get() = if (hasCreatedAt()) createdAt.seconds else 0L

/**
 * Each run as a tree: the lead (an agent with no parent in the run) on top, its workers under it, their read-only
 * agents under those. A run's agents whose parent isn't known are roots too, so nothing disappears.
 */
fun buildTrees(runs: Map<String, Run>, agents: Map<String, Agent>): List<RunTree> {
    val byRun = agents.values.groupBy { it.runId }
    val trees = runs.values.map { run ->
        val members = byRun[run.id].orEmpty()
        val known = members.mapTo(HashSet()) { it.id }
        val kids = HashMap<String, MutableList<Agent>>()
        val roots = ArrayList<Agent>()
        for (a in members) {
            if (a.parentId.isNotEmpty() && a.parentId in known) kids.getOrPut(a.parentId) { ArrayList() }.add(a) else roots.add(a)
        }
        // Built once per agent, so a parent loop in corrupt data can't recurse forever.
        val seen = HashSet<String>()
        fun node(a: Agent): TreeNode {
            seen.add(a.id)
            return TreeNode(a, kids[a.id].orEmpty().filter { it.id !in seen }.sortedWith(AgentOrder).map(::node))
        }
        val nodes = roots.sortedWith(AgentOrder).map(::node).toMutableList()
        // Agents in a parent loop are reached from no root: show each such group under its first agent.
        for (a in members.sortedWith(AgentOrder)) if (a.id !in seen) nodes.add(node(a))
        RunTree(
            run, nodes,
            needsInput = members.count { it.state == AgentState.AGENT_STATE_NEEDS_INPUT },
            live = members.count { it.state != AgentState.AGENT_STATE_EXITED && it.state != AgentState.AGENT_STATE_RETIRED },
        )
    }
    // Runs needing the user first, then open before closed, then the newest.
    return trees.sortedWith(
        compareBy<RunTree> { if (it.needsInput > 0) 0 else 1 }
            .thenBy { if (it.run.status == RunStatus.RUN_STATUS_CLOSED) 1 else 0 }
            .thenByDescending { it.run.createdSeconds },
    )
}

fun roleLabel(role: AgentRole): String = when (role) {
    AgentRole.AGENT_ROLE_ROOT -> "lead"
    AgentRole.AGENT_ROLE_WORKER -> "worker"
    AgentRole.AGENT_ROLE_ASKER -> "asker"
    AgentRole.AGENT_ROLE_REVIEWER -> "reviewer"
    else -> "agent"
}

enum class ExitKind { Finished, Stopped, Failed }

/**
 * How an exited agent ended: a lost terminal is a failure, a stop by the user or a signal is not, and otherwise the exit
 * code says (the host stores a stop as exit code -1 with its reason as the detail; -1 with nothing said is a lost terminal).
 * The desktop's `exitKind`.
 */
fun exitKind(a: Agent): ExitKind = when {
    a.stateDetail == "lost" -> ExitKind.Failed
    a.stateDetail == "killed" || a.stateDetail == "killed by signal" || a.stateDetail == "stopped" -> ExitKind.Stopped
    a.exitCode == 0 -> ExitKind.Finished
    else -> ExitKind.Failed
}

/** A plain sentence for what an agent is doing, for the tree row (the desktop's `statusSentence`). */
fun statusSentence(a: Agent): String = when (a.state) {
    AgentState.AGENT_STATE_STARTING -> "Starting up"
    AgentState.AGENT_STATE_IDLE -> "Idle, waiting for a message"
    AgentState.AGENT_STATE_WORKING -> "Working"
    AgentState.AGENT_STATE_NEEDS_INPUT -> {
        val why = if (a.stateDetail.isNotEmpty()) ": ${a.stateDetail}" else ""
        if (a.needsInput > 1) "Needs you (${a.needsInput})$why" else "Needs you$why"
    }
    AgentState.AGENT_STATE_EXITED -> when (exitKind(a)) {
        ExitKind.Stopped -> "Stopped"
        ExitKind.Finished -> "Finished"
        // The host says why when it can (`reopen failed: …`, `launch failed: …`); exit code -1 with nothing said is a lost terminal.
        ExitKind.Failed -> when {
            a.stateDetail.isNotEmpty() && a.stateDetail != "lost" && a.stateDetail != "exited" -> "Exited: ${a.stateDetail}"
            a.stateDetail == "lost" || a.exitCode < 0 -> "Lost: its terminal is gone"
            else -> "Exited with code ${a.exitCode}"
        }
    }
    AgentState.AGENT_STATE_UNKNOWN -> "Quiet for a while: state unknown"
    AgentState.AGENT_STATE_RETIRED -> "Retired: stopped on purpose, a message wakes it"
    AgentState.AGENT_STATE_LIMITED -> "Hit a usage limit${if (a.stateDetail.isNotEmpty()) ": ${a.stateDetail}" else ""}; switch it to another model to go on"
    AgentState.AGENT_STATE_ERROR -> "Its last turn ended in an error${if (a.stateDetail.isNotEmpty()) ": ${a.stateDetail}" else ""}"
    else -> "Unknown"
}

/** The last folder of a repository path. */
fun repoName(repo: String): String = repo.trimEnd('/', '\\').substringAfterLast('/').substringAfterLast('\\').ifEmpty { repo }

enum class WaitingKind { Plan, Question, Input }

/**
 * Something that waits on the user: [agent] wrote the plan or asked the question ([message]), or is stopped at a
 * prompt in its terminal (no message).
 */
data class Waiting(val kind: WaitingKind, val agent: Agent, val run: Run?, val message: Message?)

/**
 * What waits on the user (what `mesh status` calls waiting rows, for the person): a plan or a question sent to the
 * run's lead that nobody has answered since, from an agent that is still there to hear the answer, and every
 * agent stopped at a prompt in its terminal. The user may answer in the lead's place, and the lead answering
 * first closes the item. Plans and questions come oldest first, then the prompts.
 */
fun waitingOnYou(runs: Map<String, Run>, agents: Map<String, Agent>, messages: Map<String, Message>): List<Waiting> {
    val byRun = messages.values.groupBy { it.runId }
    val items = ArrayList<Waiting>()
    for (run in runs.values) {
        if (run.status != RunStatus.RUN_STATUS_OPEN) continue
        val lead = agents.values.filter { it.runId == run.id && it.role == AgentRole.AGENT_ROLE_ROOT }.sortedWith(AgentOrder).firstOrNull() ?: continue
        val msgs = byRun[run.id].orEmpty()
        for (m in msgs) {
            val kind = when (m.type) {
                MessageType.MESSAGE_TYPE_PLAN -> WaitingKind.Plan
                MessageType.MESSAGE_TYPE_QUESTION -> WaitingKind.Question
                else -> continue
            }
            if (m.toId != lead.id) continue
            val author = agents[m.from.id] ?: continue
            if (author.state == AgentState.AGENT_STATE_EXITED) continue
            val answered = msgs.any { it.type == MessageType.MESSAGE_TYPE_ANSWER && it.toId == author.id && isBefore(m, it) }
            if (!answered) items.add(Waiting(kind, author, run, m))
        }
    }
    items.sortWith { a, b -> MessageOrder.compare(a.message!!, b.message!!) }
    val prompts = agents.values
        .filter { it.state == AgentState.AGENT_STATE_NEEDS_INPUT && runs[it.runId]?.status == RunStatus.RUN_STATUS_OPEN }
        .sortedWith(AgentOrder)
        .map { Waiting(WaitingKind.Input, it, runs[it.runId], null) }
    return items + prompts
}

/** The host's `ListWaiting` rows as screens show them; a row whose agent the app doesn't know (yet) is left out. */
fun waitingFrom(list: List<dev.agentmesh.gen.agentmesh.v1.Waiting>, runs: Map<String, Run>, agents: Map<String, Agent>, onlyForUser: Boolean = true): List<Waiting> = list.mapNotNull { w ->
    // Not set for a worker's plan or question to a lead that decides it: the run's own view shows it, the user's lists do not.
    if (onlyForUser && !w.forUser) return@mapNotNull null
    val agent = agents[w.agentId] ?: return@mapNotNull null
    val kind = when (w.kind) {
        dev.agentmesh.gen.agentmesh.v1.WaitingKind.WAITING_KIND_PLAN -> WaitingKind.Plan
        dev.agentmesh.gen.agentmesh.v1.WaitingKind.WAITING_KIND_QUESTION -> WaitingKind.Question
        dev.agentmesh.gen.agentmesh.v1.WaitingKind.WAITING_KIND_INPUT -> WaitingKind.Input
        else -> return@mapNotNull null
    }
    Waiting(kind, agent, runs[w.runId.ifEmpty { agent.runId }], if (w.hasMessage()) w.message else null)
}

/**
 * What waits on the user: the host's own answer (`ListWaiting`, so both apps agree) once it has given one, and until then
 * (or on a host too old to have the action) what [waitingOnYou] derives from the messages.
 */
fun waitingFor(s: HostState): List<Waiting> = s.waiting?.let { waitingFrom(it, s.runs, s.agents) } ?: waitingOnYou(s.runs, s.agents, s.messages)

/** Everything the host says waits, the user's or not (a lead's own view of a worker's plan it decides), for the run's chat: its card is open to answer. */
fun waitingAllFor(s: HostState): List<Waiting> = s.waiting?.let { waitingFrom(it, s.runs, s.agents, onlyForUser = false) } ?: waitingOnYou(s.runs, s.agents, s.messages)

fun Agent.lastActivityMillis(): Long = if (hasLastActivity()) lastActivity.toMillis() else createdMillis

/** One line of the host screen's list: a run's header, or an agent in its run's tree. */
sealed interface Row {
    val key: String
}

class RunRow(val run: Run, val needsInput: Int, val live: Int, val expanded: Boolean) : Row {
    override val key get() = "run:${run.id}"
}

/**
 * [guides] has one entry per ancestor level: true when that ancestor has a later sibling, so the row draws a
 * vertical line there; [last] says whether this agent is its parent's last child (an elbow, not a tee).
 */
class AgentRow(val agent: Agent, val depth: Int, val guides: List<Boolean>, val last: Boolean) : Row {
    override val key get() = "agent:${agent.id}"
}

/** The trees as a flat list for a lazy column; a collapsed run shows only its header. */
fun flatten(trees: List<RunTree>, expanded: (Run) -> Boolean): List<Row> {
    val rows = ArrayList<Row>()
    for (t in trees) {
        val open = expanded(t.run)
        rows.add(RunRow(t.run, t.needsInput, t.live, open))
        if (!open) continue
        fun add(n: TreeNode, depth: Int, guides: List<Boolean>, last: Boolean) {
            rows.add(AgentRow(n.agent, depth, guides, last))
            n.children.forEachIndexed { i, c -> add(c, depth + 1, if (depth == 0) guides else guides + !last, i == n.children.lastIndex) }
        }
        t.roots.forEachIndexed { i, r -> add(r, 0, emptyList(), i == t.roots.lastIndex) }
    }
    return rows
}

/** The messages an agent sent or was sent, oldest first. */
fun messagesOf(messages: Map<String, Message>, agentId: String): List<Message> =
    messages.values.filter { it.toId == agentId || it.from.id == agentId }.sortedWith(MessageOrder)

fun messageLabel(type: MessageType): String = when (type) {
    MessageType.MESSAGE_TYPE_TASK -> "Task"
    MessageType.MESSAGE_TYPE_PLAN -> "Plan"
    MessageType.MESSAGE_TYPE_QUESTION -> "Question"
    MessageType.MESSAGE_TYPE_ANSWER -> "Answer"
    MessageType.MESSAGE_TYPE_DONE -> "Report"
    MessageType.MESSAGE_TYPE_FAILED -> "Failed"
    MessageType.MESSAGE_TYPE_INFO -> "Info"
    MessageType.MESSAGE_TYPE_WAIT -> "Waiting"
    else -> "Message"
}
