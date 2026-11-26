package dev.agentmesh.android.notify

import android.content.Context
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.hosts.SecretBox
import dev.agentmesh.android.store.ConnState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
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
 * ([enabled] false) it registers nothing.
 */
class PushRegistrar(
    private val store: EndpointStore,
    private val runtimes: StateFlow<Map<String, HostRuntime>>,
    private val enabled: StateFlow<Boolean>,
    private val scope: CoroutineScope,
) {
    private val _status = MutableStateFlow<Map<String, PushStatus>>(emptyMap())
    val status: StateFlow<Map<String, PushStatus>> get() = _status

    private val watching = HashMap<String, Job>()

    init {
        scope.launch {
            combine(runtimes, enabled) { rts, on -> if (on) rts else emptyMap() }.collect { rts ->
                synchronized(watching) {
                    for (id in watching.keys - rts.keys) watching.remove(id)?.cancel()
                    for ((id, rt) in rts) if (id !in watching) watching[id] = watch(rt)
                }
            }
        }
    }

    private fun watch(rt: HostRuntime): Job = scope.launch {
        // Ready already (push was just turned on), then every connection after.
        launch { rt.connected.collect { register(rt) } }
        when {
            rt.state.value.conn is ConnState.Ready -> register(rt)
            // A cold start keeps the endpoint from last time: what is missing is the connection.
            store.get(rt.hostId) != null && rt.hostId !in _status.value -> set(rt.hostId, PushStatus("Waiting for the host", ok = false))
        }
    }

    private fun set(hostId: String, s: PushStatus?) = _status.update { if (s == null) it - hostId else it + (hostId to s) }

    /** The distributor gave [hostId]'s instance an endpoint (a first one, or a new one). */
    fun onEndpoint(hostId: String, e: SavedEndpoint) {
        store.put(hostId, e)
        if (!enabled.value) return
        val rt = runtimes.value[hostId] ?: return
        set(hostId, PushStatus("Waiting for the host", ok = false))
        if (rt.state.value.conn is ConnState.Ready) scope.launch { register(rt) }
    }

    private suspend fun register(rt: HostRuntime) {
        val e = store.get(rt.hostId) ?: return set(rt.hostId, PushStatus("Waiting for the distributor", ok = false))
        val api = rt.api() ?: return
        try {
            api.registerPush(e.url, e.p256dh, e.auth)
            set(rt.hostId, PushStatus("Registered with the host", ok = true))
        } catch (ex: Exception) {
            set(rt.hostId, PushStatus("The host refused it: ${ex.message ?: ex.javaClass.simpleName}", ok = false))
        }
    }

    /** The distributor couldn't register [hostId]'s instance. */
    fun failed(hostId: String, why: String) = set(hostId, PushStatus(why, ok = false))

    /** The instance is gone (unregistered here or by the distributor): forget its endpoint. */
    fun forget(hostId: String) {
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

    /** Tells the host to stop pushing here, if it can be reached now (else it learns when the endpoint is gone). */
    suspend fun unregisterAt(hostId: String) {
        runCatching { runtimes.value[hostId]?.api()?.unregisterPush() }
    }
}
