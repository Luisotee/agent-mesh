package dev.agentmesh.android.derive

import dev.agentmesh.android.store.MessageOrder
import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.Run
import dev.agentmesh.gen.agentmesh.v1.RunStatus

/** One live agent of the dock: what it is doing now and the last thing it said. The desktop's `dock.ts`. */
class Tile(
    val agent: Agent,
    val run: Run?,
    /** The tool the agent is running now, while it works. */
    val activity: String,
    /** The last message the agent sent that the app has seen. */
    val lastSent: Message?,
)

private fun alive(a: Agent) = a.state != AgentState.AGENT_STATE_EXITED && a.state != AgentState.AGENT_STATE_RETIRED
private fun attention(a: Agent) = a.state == AgentState.AGENT_STATE_NEEDS_INPUT || a.state == AgentState.AGENT_STATE_ERROR || a.state == AgentState.AGENT_STATE_LIMITED
private fun at(a: Agent) = if (a.hasLastActivity()) a.lastActivity.toMillis() else 0L

/** An agent's last activity as epoch milliseconds in the host's clock (0 when it has none). */
fun activityMillis(a: Agent) = at(a)

/** A tile per live agent of the open runs: those that need the user first, then the rest by latest activity. */
fun dockTiles(runs: Map<String, Run>, agents: Map<String, Agent>, messages: Map<String, Message>): List<Tile> {
    val lastSent = HashMap<String, Message>()
    for (m in messages.values) {
        val from = m.from.id
        if (from.isEmpty()) continue
        val had = lastSent[from]
        if (had == null || MessageOrder.compare(had, m) < 0) lastSent[from] = m
    }
    val tiles = agents.values.mapNotNull { a ->
        val run = runs[a.runId]
        if (!alive(a) || run?.status == RunStatus.RUN_STATUS_CLOSED) null
        else Tile(a, run, if (a.state == AgentState.AGENT_STATE_WORKING) a.stateDetail else "", lastSent[a.id])
    }
    return tiles.sortedWith(compareBy<Tile> { if (attention(it.agent)) 0 else 1 }.thenByDescending { at(it.agent) }.thenBy { it.agent.name })
}

/** "sent a plan", for the last thing an agent said. */
fun sentLabel(m: Message): String = when (m.type) {
    MessageType.MESSAGE_TYPE_PLAN -> "sent a plan"
    MessageType.MESSAGE_TYPE_QUESTION -> "asked a question"
    MessageType.MESSAGE_TYPE_DONE -> "reported done"
    MessageType.MESSAGE_TYPE_FAILED -> "reported a failure"
    MessageType.MESSAGE_TYPE_TASK -> "sent a task"
    MessageType.MESSAGE_TYPE_ANSWER -> "sent an answer"
    else -> "sent a message"
}

/** How long ago, short: "now", "12 s", "4 min", "3 h", "2 d". */
fun ago(ms: Long, now: Long): String {
    if (ms <= 0) return ""
    val s = maxOf(0L, Math.round((now - ms) / 1000.0))
    return when {
        s < 5 -> "now"
        s < 60 -> "$s s"
        s < 3600 -> "${s / 60} min"
        s < 86_400 -> "${s / 3600} h"
        else -> "${s / 86_400} d"
    }
}
