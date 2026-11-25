package dev.agentmesh.android.notify

import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.ActorClass
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.Event
import dev.agentmesh.gen.agentmesh.v1.MessageType

/** What a notification is about; the host's `app.notify.events` setting names these (docs/apps.md, "Notifications"). */
enum class NoticeKind(val key: String) { Input("input"), Plan("plan"), Question("question"), Report("report"), Failed("failed"), Exited("exited") }

/** The screen a notification opens. */
enum class Tab { Overview, Messages, Terminal }

class Notice(
    val kind: NoticeKind,
    val title: String,
    val body: String,
    val agentId: String,
    val tab: Tab,
)

/** An event older than this wasn't "just now": history replayed after a reconnect doesn't notify. */
const val LIVE_WITHIN_MS = 60_000L

private const val MAX_BODY = 200

/** A notification is plain text: drop the Markdown's own marks (headings, bullets, emphasis, code ticks). */
fun brief(text: String): String {
    val flat = text.lines().joinToString(" ") { it.replace(Regex("""^\s*(#{1,6}\s+|>\s*|[-*+]\s+|\d+\.\s+)"""), "") }
        .replace(Regex("[*`]+"), "").replace(Regex("\\s+"), " ").trim()
    return if (flat.length > MAX_BODY) flat.take(MAX_BODY - 1) + "…" else flat
}

/**
 * What an event means to the person, if anything: an agent stopped at a prompt, a plan, a question, a report or a
 * failure sent to a run's lead, an agent that died. Only live events (history replayed after a reconnect stays
 * quiet), and never what the user did themselves. [agents] is the state before the event; [clockSkewMs] is how far
 * the host's clock is ahead of ours. The same rules as the desktop's `notify.ts`.
 */
fun noticeFor(e: Event, agents: Map<String, Agent>, nowMs: Long, clockSkewMs: Long = 0): Notice? {
    val at = if (e.hasTime()) e.time.toMillis() else 0L
    // The event's time is the host's: compare it with the host's clock now, not ours.
    if (nowMs + clockSkewMs - at > LIVE_WITHIN_MS) return null
    when (e.payloadCase) {
        Event.PayloadCase.AGENT_STATE_CHANGED -> {
            val a = agents[e.agentId] ?: return null
            val ch = e.agentStateChanged
            if (ch.to != AgentState.AGENT_STATE_NEEDS_INPUT || ch.from == AgentState.AGENT_STATE_NEEDS_INPUT) return null
            return Notice(NoticeKind.Input, "${a.name} needs you", brief(ch.detail).ifEmpty { "It is stopped at a prompt in its terminal." }, a.id, Tab.Terminal)
        }
        Event.PayloadCase.MESSAGE_SENT -> {
            val m = e.messageSent.message
            if (m.from.`class_` == ActorClass.ACTOR_CLASS_DEVICE) return null
            val to = agents[m.toId]
            val from = agents[m.from.id]
            // What reaches a run's lead is what the user is waiting for; talk among workers isn't.
            if (to == null || to.role != AgentRole.AGENT_ROLE_ROOT || from == null) return null
            val body = brief(m.body)
            return when (m.type) {
                MessageType.MESSAGE_TYPE_PLAN -> Notice(NoticeKind.Plan, "${from.name} sent a plan", body, from.id, Tab.Messages)
                MessageType.MESSAGE_TYPE_QUESTION -> Notice(NoticeKind.Question, "${from.name} has a question", body, from.id, Tab.Messages)
                MessageType.MESSAGE_TYPE_DONE -> Notice(NoticeKind.Report, "${from.name} reported done", body, from.id, Tab.Messages)
                MessageType.MESSAGE_TYPE_FAILED -> Notice(NoticeKind.Failed, "${from.name} failed", body, from.id, Tab.Messages)
                else -> null
            }
        }
        Event.PayloadCase.AGENT_EXITED -> {
            val a = agents[e.agentId] ?: return null
            val x = e.agentExited
            // The user stopping an agent isn't news.
            if (x.reason == "killed") return null
            val clean = x.exitCode == 0 && x.reason != "lost"
            val title = when {
                clean -> "${a.name} finished"
                x.reason == "lost" -> "${a.name} was lost"
                else -> "${a.name} exited with code ${x.exitCode}"
            }
            return Notice(if (clean) NoticeKind.Exited else NoticeKind.Failed, title, "", a.id, Tab.Overview)
        }
        else -> return null
    }
}
