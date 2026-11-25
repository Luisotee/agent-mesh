package dev.agentmesh.android.store

import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.Cursor
import dev.agentmesh.gen.agentmesh.v1.Event
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.Run
import dev.agentmesh.gen.agentmesh.v1.Setting
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse

/** The state of a host's connection, as the user sees it. */
sealed interface ConnState {
    data object Connecting : ConnState
    data class Ready(val hello: HelloResponse) : ConnState

    /** Not connected; trying again at [retryAtMs] (ms since the epoch). */
    data class Reconnecting(val attempt: Int, val retryAtMs: Long, val error: String) : ConnState

    /** Won't retry until asked: the host refused the token or speaks another protocol, or the route can't be used. */
    data class Stopped(val reason: Reason, val message: String) : ConnState

    enum class Reason { Auth, Incompatible, Config }
}

data class LogLine(val atMs: Long, val text: String)

/** One host as the app knows it: what its snapshot and event stream said, and the state of the connection. */
data class HostState(
    val conn: ConnState = ConnState.Connecting,
    /** What the host last said about itself, kept while it is unreachable so the view doesn't change under the user. */
    val daemon: HelloResponse? = null,
    /** How far the host's clock is ahead of ours, in ms (negative: behind), from its last `Hello`. */
    val clockSkewMs: Long = 0,
    /** True once a snapshot or a resume arrived on the current connection. */
    val synced: Boolean = false,
    val runs: Map<String, Run> = emptyMap(),
    val agents: Map<String, Agent> = emptyMap(),
    /** Every message seen: loaded with ListMessages, then added by events. */
    val messages: Map<String, Message> = emptyMap(),
    /** The host's settings with their schemas, by key. */
    val settings: Map<String, Setting> = emptyMap(),
    val cursor: Cursor? = null,
    val log: List<LogLine> = emptyList(),
)

const val MAX_LOG = 200

typealias Frame = SubscribeEventsResponse

/**
 * Applies one frame of the event stream. A snapshot replaces runs and agents (a resync); a resume keeps them; an
 * event changes one thing. Pure, so the stream and its replay can be checked to give the same state.
 */
fun applyFrame(s: HostState, frame: Frame): HostState = when (frame.frameCase) {
    SubscribeEventsResponse.FrameCase.SNAPSHOT -> {
        val snap = frame.snapshot
        val runs = snap.runsList.associateBy { it.id }
        val agents = snap.agentsList.associateBy { it.id }
        // Messages of runs the host no longer lists go; those of the others stay (and are topped up by the loader).
        s.copy(synced = true, runs = runs, agents = agents, messages = s.messages.filterValues { it.runId in runs }, cursor = snap.cursor)
    }
    SubscribeEventsResponse.FrameCase.RESUMED -> s.copy(synced = true)
    SubscribeEventsResponse.FrameCase.EVENT -> applyEvent(s, frame.event)
    else -> s
}

private fun applyEvent(s: HostState, e: Event): HostState {
    val next = s.copy(cursor = Cursor.newBuilder().setGeneration(e.generation).setSeq(e.seq).build())
    return when (e.payloadCase) {
        Event.PayloadCase.RUN_STARTED -> if (e.runStarted.hasRun()) next.copy(runs = s.runs + (e.runStarted.run.id to e.runStarted.run)) else next
        Event.PayloadCase.MESSAGE_SENT -> if (e.messageSent.hasMessage()) next.copy(messages = s.messages + (e.messageSent.message.id to e.messageSent.message)) else next
        Event.PayloadCase.MESSAGE_DELIVERED -> {
            val m = s.messages[e.messageDelivered.messageId]
            if (m == null) next else next.copy(messages = s.messages + (m.id to m.toBuilder().apply { if (e.hasTime()) setDeliveredAt(e.time) }.build()))
        }
        Event.PayloadCase.SETTING_CHANGED -> {
            val ch = e.settingChanged
            val cur = s.settings[ch.key] ?: return next
            // The event carries the effective value now; a reset means the default applies again.
            val b = cur.toBuilder().setIsSet(!ch.toDefault)
            if (ch.hasValue()) b.setValue(ch.value) else if (ch.toDefault) b.setValue(cur.default)
            if (e.hasActor()) b.setUpdatedBy(e.actor)
            if (e.hasTime()) b.setUpdatedAt(e.time)
            next.copy(settings = s.settings + (cur.key to b.build()))
        }
        Event.PayloadCase.AGENT_SPAWNED -> if (e.agentSpawned.hasAgent()) next.copy(agents = s.agents + (e.agentSpawned.agent.id to e.agentSpawned.agent)) else next
        Event.PayloadCase.AGENT_STATE_CHANGED -> {
            val a = s.agents[e.agentId] ?: return next
            val ch = e.agentStateChanged
            val b = a.toBuilder().setState(ch.to).setStateDetail(ch.detail).setNeedsInput(ch.needsInput)
            if (e.hasTime()) b.setLastActivity(e.time)
            next.copy(agents = s.agents + (a.id to b.build()))
        }
        Event.PayloadCase.AGENT_EXITED -> {
            val a = s.agents[e.agentId] ?: return next
            val b = a.toBuilder().setState(AgentState.AGENT_STATE_EXITED).setExitCode(e.agentExited.exitCode)
                .setStateDetail(e.agentExited.reason).setNeedsInput(0)
            if (e.hasTime()) b.setLastActivity(e.time)
            next.copy(agents = s.agents + (a.id to b.build()))
        }
        Event.PayloadCase.AGENT_ADOPTED -> {
            val a = s.agents[e.agentId] ?: return next
            next.copy(agents = s.agents + (a.id to a.toBuilder().setTerminalId(e.agentAdopted.terminalId).build()))
        }
        else -> next
    }
}

/** Adds messages (a loaded page) to the ones we have; one we already have, which events may have updated, is kept. */
fun mergeMessages(s: HostState, page: List<Message>): HostState {
    val merged = s.messages.toMutableMap()
    for (m in page) merged.putIfAbsent(m.id, m)
    return s.copy(messages = merged)
}

/** A fresh listing of settings replaces what we have (the schema can differ after a daemon update). */
fun withSettings(s: HostState, list: List<Setting>): HostState = s.copy(settings = list.associateBy { it.key })
