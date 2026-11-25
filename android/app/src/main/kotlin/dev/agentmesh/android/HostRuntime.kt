package dev.agentmesh.android

import com.google.protobuf.Value
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
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import java.util.concurrent.atomic.AtomicInteger

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
) : ConnectionSink {
    private val _state = MutableStateFlow(HostState())
    val state: StateFlow<HostState> get() = _state

    /** The route in use: the one that last worked, and the next after each failure. */
    private val route = AtomicInteger(0)
    @Volatile private var current: MeshApi? = null

    val connection = HostConnection(scope, ::buildApi, this, now, random, heartbeatMs = heartbeatMs)
    private var watcher: Job? = null

    val host: Host? get() = repo.get(hostId)

    private fun buildApi(): MeshApi {
        val h = host ?: throw RouteException("this host was removed")
        if (h.routes.isEmpty()) throw RouteException("this host has no route")
        // A token the Keystore can no longer open (a restored backup, a new lock screen) is the user's to re-enter.
        if (repo.token(hostId) == null) throw RouteException("this device can't read the saved token: edit the host and enter it again")
        val r = h.routes[route.get() % h.routes.size]
        val api = newApi(r) { repo.token(hostId) ?: "" }
        current = api
        return api
    }

    fun start() {
        connection.start()
        // The host's settings come once a connection is synced.
        watcher = scope.launch {
            _state.map { it.synced && it.conn is ConnState.Ready }.distinctUntilChanged().filter { it }.collect { loadSettings() }
        }
    }

    fun stop() {
        connection.stop()
        watcher?.cancel()
    }

    fun nudge() = connection.nudge()

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

    // ── ConnectionSink ──

    override fun onState(state: ConnState) {
        if (state is ConnState.Reconnecting) route.incrementAndGet()
        _state.update {
            when (state) {
                is ConnState.Ready -> it.copy(conn = state, daemon = state.hello)
                else -> it.copy(conn = state, synced = false)
            }
        }
    }

    override fun onHello(hello: HelloResponse) {
        val skew = if (hello.hasServerTime()) hello.serverTime.seconds * 1000 + hello.serverTime.nanos / 1_000_000 - now() else null
        val known = host?.daemonId
        if (known != null && known != hello.daemonId) {
            // Another daemon answers at this address: nothing we hold is about it.
            log("the host at this address is a different daemon ($known before, ${hello.daemonId} now): starting over")
            _state.update { HostState(conn = it.conn, daemon = hello, log = it.log) }
        }
        if (known != hello.daemonId) repo.setDaemonId(hostId, hello.daemonId)
        if (skew != null) _state.update { it.copy(clockSkewMs = skew) }
    }

    override fun onFrame(frame: Frame) = _state.update { applyFrame(it, frame) }

    override fun cursor(): Cursor? = _state.value.cursor

    override fun log(line: String) {
        _state.update { it.copy(log = (it.log + LogLine(now(), line)).takeLast(MAX_LOG)) }
    }
}
