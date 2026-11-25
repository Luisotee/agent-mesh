package dev.agentmesh.android

import com.google.protobuf.Value
import dev.agentmesh.android.notify.Notice
import dev.agentmesh.android.notify.noticeFor
import dev.agentmesh.android.store.MessageLoader
import dev.agentmesh.android.store.mergeMessages
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentPhase
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import kotlinx.coroutines.flow.first
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.MessageType
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow
import dev.agentmesh.android.hosts.Host
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.ConnectionSink
import dev.agentmesh.android.net.HostConnection
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.net.RouteException
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.store.Frame
import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.store.LogLine
import dev.agentmesh.android.store.MAX_LOG
import dev.agentmesh.android.store.applyFrame
import dev.agentmesh.android.store.withSettings
import dev.agentmesh.gen.agentmesh.v1.Cursor
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.Setting
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import java.util.concurrent.atomic.AtomicInteger

/** The plan or question the action is about was answered meanwhile (by the lead or on another device). */
class AlreadyAnswered : Exception("This was already answered")

/** The agent has no plan or question waiting for an answer, so there is nothing for this action to answer. */
class NothingToAnswer(message: String) : Exception(message)

/** The app doesn't know this agent's conversation (yet), so it can't tell what an answer would answer. */
class NotLoaded(message: String) : Exception(message)

/**
 * One host as the app runs it: its connection, and the state built only from that connection's snapshot and
 * events. The UI reads [state] and calls [api] for its actions; nothing else reaches the host.
 */
class HostRuntime(
    val hostId: String,
    private val repo: HostRepository,
    /** The client for one route; may throw [RouteException]. The token is read when a request is made. */
    private val newApi: (Route, () -> String) -> MeshApi,
    private val scope: CoroutineScope,
    private val now: () -> Long = System::currentTimeMillis,
    random: () -> Double = Math::random,
    heartbeatMs: Long = 15_000,
    messageRetryMs: Long = 1_000,
) : ConnectionSink {
    private val _state = MutableStateFlow(HostState())
    val state: StateFlow<HostState> get() = _state

    /** The route in use: the one that last worked, and the next after each failure. */
    private val route = AtomicInteger(0)
    @Volatile private var current: MeshApi? = null

    private val _notices = MutableSharedFlow<Notice>(extraBufferCapacity = 64, onBufferOverflow = BufferOverflow.DROP_OLDEST)

    /** What just happened that the user may want to know: live events only (see [noticeFor]). */
    val notices: SharedFlow<Notice> get() = _notices

    private val loader = MessageLoader(scope, { current }, { _state.value }, { f -> _state.update(f) }, ::log, messageRetryMs)

    val connection = HostConnection(scope, ::buildApi, this, now, random, heartbeatMs = heartbeatMs)
    private var watcher: Job? = null

    val host: Host? get() = repo.get(hostId)

    private fun buildApi(): MeshApi {
        val h = host ?: throw RouteException("this host was removed")
        if (h.routes.isEmpty()) throw RouteException("this host has no route")
        // A token the Keystore can no longer open (a restored backup, a new lock screen) is the user's to re-enter.
        if (repo.token(hostId) == null) throw RouteException("this device can't read the saved token: edit the host and enter it again")
        // The route that last worked, then the others in order: one that can't be used at all (a bad address) is
        // skipped here, so it doesn't stop the host while another route would do.
        var firstError: RouteException? = null
        for (i in h.routes.indices) {
            val r = h.routes[(route.get() + i) % h.routes.size]
            try {
                val api = newApi(r) { repo.token(hostId) ?: "" }
                route.set((route.get() + i) % h.routes.size)
                current = api
                return api
            } catch (e: RouteException) {
                if (firstError == null) firstError = e
            }
        }
        throw firstError!!
    }

    fun start() {
        connection.start()
        // The host's settings come once a connection is synced.
        watcher = scope.launch {
            _state.map { it.synced && it.conn is ConnState.Ready }.distinctUntilChanged().filter { it }.collect {
                loadMessages()
                loadSettings()
            }
        }
    }

    fun stop() {
        connection.stop()
        watcher?.cancel()
    }

    fun nudge() = connection.nudge()

    /** Load the stored messages of every open run (a synced connection does it by itself). */
    fun loadMessages() = loader.load(_state.value.runs.values)

    /** Try again the runs whose load gave up; the others are left as they are. */
    fun reloadMessages() = loader.load(_state.value.runs.values.filter { it.id in _state.value.messagesFailed })

    /** Reconnect now, on the host's current routes and token (they were edited). */
    fun reconnect() = connection.restart()

    /** The client for an action, or null while no attempt has built one. */
    fun api(): MeshApi? = current

    private suspend fun loadSettings() {
        val api = current ?: return
        try {
            val list = api.listSettings()
            _state.update { withSettings(it, list) }
        } catch (e: ApiException) {
            log("couldn't load the host's settings: ${e.message}")
        }
    }

    suspend fun setSetting(key: String, value: Value): Setting = checkNotNull(current) { "not connected" }.setSetting(key, value).also { s ->
        _state.update { it.copy(settings = it.settings + (s.key to s)) }
    }

    suspend fun resetSetting(key: String): Setting = checkNotNull(current) { "not connected" }.resetSetting(key).also { s ->
        _state.update { it.copy(settings = it.settings + (s.key to s)) }
    }

    // ── What the user does: each is an action of the registry, and its stored message joins the state at once ──

    /** The client once the connection is ready (a terminal attaches only to a host that answered). */
    suspend fun awaitApi(): MeshApi {
        _state.first { it.conn is ConnState.Ready }
        return live()
    }

    private fun live(): MeshApi = current ?: throw IllegalStateException("Not connected to the host")

    private fun stored(m: Message): Message {
        _state.update { mergeMessages(it, listOf(m)) }
        return m
    }

    suspend fun send(to: String, type: MessageType, body: String): Message = stored(live().sendMessage(to, type, body))

    /**
     * Approves the plan (or question) an agent is waiting on. A planning worker moves to its execution model in the
     * same conversation (`ApproveAgent`); any other agent gets the answer "approved". With nothing waiting on an
     * answer (it was answered, or the run is closed and its messages aren't loaded) it throws [AlreadyAnswered] or
     * [NothingToAnswer] and sends nothing; while the conversation isn't known, [NotLoaded].
     */
    suspend fun approve(agentId: String): Message {
        unansweredAsk(agentId)
        val planning = _state.value.agents[agentId]?.phase == AgentPhase.AGENT_PHASE_PLANNING
        return stored(if (planning) live().approveAgent(agentId) else live().sendMessage(agentId, MessageType.MESSAGE_TYPE_ANSWER, "approved"))
    }

    /**
     * Waits until this connection has the agent's conversation: the snapshot is in and its run's messages loaded.
     * A notification's action may run in a process that was just started, where the state is still empty and a
     * guard would see no plan at all. Throws [NotLoaded] when the run's messages can't be loaded.
     */
    suspend fun awaitFresh(agentId: String) {
        _state.first { it.synced && it.conn is ConnState.Ready && (it.agents[agentId]?.let { a -> historyKnown(it, a) } ?: true) }
        notLoaded(_state.value, agentId)?.let { throw NotLoaded(it) }
    }

    /**
     * Why an action on [agentId] is waiting, for a message when it gave up: its conversation couldn't be loaded
     * (or is still loading) on a connection that is up; null when the host itself can't be reached.
     */
    fun waitingReason(agentId: String): String? {
        val s = _state.value
        if (s.conn !is ConnState.Ready || !s.synced) return null
        return notLoaded(s, agentId)
    }

    private fun historyKnown(s: HostState, a: Agent): Boolean {
        val run = s.runs[a.runId]
        return run == null || run.status != RunStatus.RUN_STATUS_OPEN || a.runId in s.messagesLoaded || a.runId in s.messagesFailed
    }

    /** Why the conversation of [agentId] can't be relied on right now, or null when it can. */
    private fun notLoaded(s: HostState, agentId: String): String? {
        val a = s.agents[agentId] ?: return if (s.synced) "That agent isn't on the host any more." else "Not synced with the host yet."
        if (!s.synced) return "Not synced with the host yet."
        if (a.runId in s.messagesFailed) return "Couldn't load this agent's messages. Open the app to answer there."
        if (!historyKnown(s, a)) return "Still loading this agent's messages. Try again in a moment."
        return null
    }

    /**
     * The agent's latest plan or question when nobody has answered it since (the user on another device, the lead):
     * a notification's Approve or Reply on anything else is stale and must not send a second answer. Throws
     * [NotLoaded] while the conversation isn't known, and [AlreadyAnswered] when there is nothing to answer.
     */
    private fun unansweredAsk(agentId: String): Message {
        val s = _state.value
        notLoaded(s, agentId)?.let { throw NotLoaded(it) }
        val msgs = s.messages.values
        val ask = msgs.filter { it.from.id == agentId && (it.type == MessageType.MESSAGE_TYPE_PLAN || it.type == MessageType.MESSAGE_TYPE_QUESTION) }
            .maxWithOrNull(dev.agentmesh.android.store.MessageOrder)
            ?: throw NothingToAnswer("${s.agents[agentId]?.name ?: "The agent"} has no plan or question waiting for an answer, so nothing was sent.")
        if (msgs.any { it.toId == agentId && it.type == MessageType.MESSAGE_TYPE_ANSWER && dev.agentmesh.android.store.isBefore(ask, it) }) throw AlreadyAnswered()
        return ask
    }

    suspend fun requestChanges(agentId: String, text: String): Message {
        unansweredAsk(agentId)
        return send(agentId, MessageType.MESSAGE_TYPE_ANSWER, "changes: ${text.trim()}")
    }

    suspend fun answer(agentId: String, text: String): Message {
        unansweredAsk(agentId)
        return send(agentId, MessageType.MESSAGE_TYPE_ANSWER, text.trim())
    }

    // ── ConnectionSink ──

    override fun onState(state: ConnState) {
        // Another route only after a real failure to connect: an End frame (attempt 0) is the host restarting.
        if (state is ConnState.Reconnecting && state.attempt > 0) route.incrementAndGet()
        _state.update {
            when (state) {
                is ConnState.Ready -> it.copy(conn = state, daemon = state.hello)
                else -> it.copy(conn = state, synced = false, messagesLoaded = emptySet(), messagesFailed = emptySet())
            }
        }
    }

    override fun onHello(hello: HelloResponse) {
        val skew = if (hello.hasServerTime()) hello.serverTime.seconds * 1000 + hello.serverTime.nanos / 1_000_000 - now() else null
        val known = host?.daemonId
        if (known != null && known != hello.daemonId) {
            // Another daemon answers at this address: nothing we hold is about it.
            log("the host at this address is a different daemon ($known before, ${hello.daemonId} now): starting over")
            loader.reset()
            _state.update { HostState(conn = it.conn, daemon = hello, log = it.log) }
        }
        if (known != hello.daemonId) repo.setDaemonId(hostId, hello.daemonId)
        if (skew != null) _state.update { it.copy(clockSkewMs = skew) }
    }

    override fun onFrame(frame: Frame) {
        if (frame.hasEvent()) {
            val s = _state.value
            noticeFor(frame.event, s.agents, now(), s.clockSkewMs)?.let { _notices.tryEmit(it) }
        }
        _state.update { applyFrame(it, frame) }
    }

    override fun cursor(): Cursor? = _state.value.cursor

    override fun log(line: String) {
        _state.update { it.copy(log = (it.log + LogLine(now(), line)).takeLast(MAX_LOG)) }
    }
}
