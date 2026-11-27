package dev.agentmesh.android.notify

import android.content.Context
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.hosts.SecretBox
import dev.agentmesh.android.store.ConnState
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

/** A host's push endpoint as the distributor gave it, with this device's WebPush keys (base64url). */
@Serializable
data class SavedEndpoint(val url: String, val p256dh: String, val auth: String) {
    // The URL is a capability (whoever has it can push to this phone): never in a log.
    override fun toString() = "SavedEndpoint(…)"
}

/** Where each host's endpoint is kept between process starts. */
interface EndpointStore {
    fun get(hostId: String): SavedEndpoint?
    fun put(hostId: String, e: SavedEndpoint)
    fun remove(hostId: String)
}

/** Shared preferences, each endpoint sealed with the Keystore key that seals the hosts' tokens. */
class PrefsEndpointStore(context: Context, private val secrets: SecretBox) : EndpointStore {
    private val prefs = context.getSharedPreferences("push", Context.MODE_PRIVATE)

    override fun get(hostId: String): SavedEndpoint? = prefs.getString(hostId, null)?.let {
        runCatching { Json.decodeFromString<SavedEndpoint>(secrets.open(it)) }.getOrNull()
    }

    override fun put(hostId: String, e: SavedEndpoint) = prefs.edit().putString(hostId, secrets.seal(Json.encodeToString(SavedEndpoint.serializer(), e))).apply()

    override fun remove(hostId: String) = prefs.edit().remove(hostId).apply()
}

/** How push stands with one host, for the settings screen. */
data class PushStatus(val text: String, val ok: Boolean)

/**
 * Gives each host this device's push endpoint: when the distributor hands one over, and again on every connection
 * that becomes ready (so a changed endpoint, or a host that lost it, heals on the next connect). While push is off
 * ([enabled] false) it registers nothing. A host's RegisterPush and UnregisterPush calls go one at a time, in the
 * order they were asked for, so a slow call can't land after a newer one and undo it.
 */
class PushRegistrar(
    private val store: EndpointStore,
    private val runtimes: StateFlow<Map<String, HostRuntime>>,
    private val enabled: StateFlow<Boolean>,
    /** The kinds this device chose to be pushed (null: each host's own list), sent with every registration. */
    private val events: StateFlow<Set<String>?>,
    private val scope: CoroutineScope,
) {
    private val _status = MutableStateFlow<Map<String, PushStatus>>(emptyMap())
    val status: StateFlow<Map<String, PushStatus>> get() = _status

    private val watching = HashMap<String, Job>()

    /** Each host's turn for its push calls (a fair mutex: first asked, first sent). */
    private val turns = HashMap<String, Mutex>()

    /** The registration [onEndpoint] started per host, cancelled when push goes off or the host is forgotten. */
    private val pending = HashMap<String, Job>()

    private fun turn(hostId: String) = synchronized(turns) { turns.getOrPut(hostId) { Mutex() } }

    init {
        // A changed choice goes to every host at once (a registration replaces the earlier one's list).
        scope.launch {
            events.drop(1).collect {
                // One coroutine per host: a host that is slow to answer doesn't hold the others' new list back. A host that isn't
                // connected now registers with the current list when it connects (each registration reads it afresh).
                for (rt in runtimes.value.values) if (rt.state.value.conn is ConnState.Ready && store.get(rt.hostId) != null) launch { register(rt) }
            }
        }
        scope.launch {
            combine(runtimes, enabled) { rts, on -> if (on) rts else emptyMap() }.collect { rts ->
                synchronized(watching) {
                    for (id in watching.keys - rts.keys) watching.remove(id)?.cancel()
                    for ((id, rt) in rts) if (id !in watching) watching[id] = watch(rt)
                }
                if (rts.isEmpty()) synchronized(pending) { pending.values.forEach { it.cancel() }; pending.clear() }
            }
        }
    }

    private fun watch(rt: HostRuntime): Job = scope.launch {
        // Subscribed before the state is read, so a connection that becomes ready in between isn't missed.
        launch(start = CoroutineStart.UNDISPATCHED) { rt.connected.collect { register(rt) } }
        when {
            rt.state.value.conn is ConnState.Ready -> register(rt)
            // A cold start keeps the endpoint from last time: what is missing is the connection.
            store.get(rt.hostId) != null && rt.hostId !in _status.value -> set(rt.hostId, PushStatus("Waiting for the host", ok = false))
        }
    }

    /** Sets a host's status; a status (not a removal) only while push is on. */
    private fun set(hostId: String, s: PushStatus?) {
        if (s != null && !enabled.value) return
        _status.update { if (s == null) it - hostId else it + (hostId to s) }
    }

    /** The distributor gave [hostId]'s instance an endpoint (a first one, or a new one). */
    fun onEndpoint(hostId: String, e: SavedEndpoint) {
        store.put(hostId, e)
        if (!enabled.value) return
        val rt = runtimes.value[hostId] ?: return
        set(hostId, PushStatus("Waiting for the host", ok = false))
        if (rt.state.value.conn !is ConnState.Ready) return
        synchronized(pending) {
            pending.remove(hostId)?.cancel()
            // Undispatched: it takes its turn now, behind a call already asked for.
            pending[hostId] = scope.launch(start = CoroutineStart.UNDISPATCHED) { register(rt) }
        }
    }

    private suspend fun register(rt: HostRuntime) {
        if (store.get(rt.hostId) == null) return set(rt.hostId, PushStatus("Waiting for the distributor", ok = false))
        turn(rt.hostId).withLock {
            // Read again after waiting: push may have gone off, or the endpoint changed, meanwhile.
            val e = store.get(rt.hostId)
            if (!enabled.value || e == null) return
            val api = rt.api() ?: return
            val st = try {
                api.registerPush(e.url, e.p256dh, e.auth, events.value.orEmpty().sorted())
                PushStatus("Registered with the host", ok = true)
            } catch (ex: CancellationException) {
                throw ex
            } catch (ex: Exception) {
                PushStatus("The host refused it: ${ex.message ?: ex.javaClass.simpleName}", ok = false)
            }
            if (store.get(rt.hostId) == e) set(rt.hostId, st)
        }
    }

    /** The distributor couldn't register [hostId]'s instance. */
    fun failed(hostId: String, why: String) = set(hostId, PushStatus(why, ok = false))

    /** The instance is gone (unregistered here or by the distributor): forget its endpoint. */
    fun forget(hostId: String) {
        synchronized(pending) { pending.remove(hostId)?.cancel() }
        store.remove(hostId)
        set(hostId, null)
    }

    /** Asks the host for a test push now. Returns the error to show, or null when the push service took it. */
    suspend fun test(hostId: String): String? {
        val api = runtimes.value[hostId]?.api() ?: return "Not connected to the host"
        return try {
            api.testPush()
            null
        } catch (ex: Exception) {
            ex.message ?: ex.javaClass.simpleName
        }
    }

    /**
     * Tells each host to stop pushing here, all at once in the background, if it can be reached now (else it learns
     * when the endpoint is gone). Each call takes its host's turn at once, so a registration asked for later goes
     * after it, and one already being sent goes before it.
     */
    fun unregisterAt(hostIds: List<String>) {
        for (id in hostIds) scope.launch(start = CoroutineStart.UNDISPATCHED) {
            turn(id).withLock { runCatching { runtimes.value[id]?.api()?.unregisterPush() } }
        }
    }
}
