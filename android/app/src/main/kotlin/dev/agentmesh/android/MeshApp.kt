package dev.agentmesh.android

import android.app.Application
import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.hosts.KeystoreSecretBox
import dev.agentmesh.android.net.ConnectMeshApi
import dev.agentmesh.android.net.DefaultRouteConnector
import dev.agentmesh.android.net.RouteConnector
import dev.agentmesh.android.notify.DeepLink
import dev.agentmesh.android.notify.EndpointStore
import dev.agentmesh.android.notify.Notice
import dev.agentmesh.android.notify.Notifier
import dev.agentmesh.android.notify.PrefsEndpointStore
import dev.agentmesh.android.notify.PushChannel
import dev.agentmesh.android.notify.PushPayload
import dev.agentmesh.android.notify.PushRegistrar
import dev.agentmesh.android.notify.UnifiedPushChannel
import dev.agentmesh.android.notify.notifies
import dev.agentmesh.android.theme.ThemeStore
import dev.agentmesh.android.theme.followedTheme
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.flowOf
import kotlinx.coroutines.launch
import java.io.File

/**
 * What lives as long as the process: the host list and one [HostRuntime] per host, kept connected while the app
 * is in front (a foreground service for the background comes with notifications). The connection hints
 * (the app came to the foreground, a network came up) go to every host.
 */
class AppContainer(
    context: Context,
    secrets: dev.agentmesh.android.hosts.SecretBox,
    connector: RouteConnector = DefaultRouteConnector(),
    /** The client for a route; tests put a fake host here. */
    private val newApi: (dev.agentmesh.android.net.Route, () -> String) -> dev.agentmesh.android.net.MeshApi =
        { route, token -> ConnectMeshApi(connector.connect(route, token)) },
    /** How a pairing call reaches a route; tests put a fake host here. */
    pairingClients: dev.agentmesh.android.hosts.PairingClients = dev.agentmesh.android.hosts.PairingClients.Default,
    /** The UnifiedPush distributor; tests put a fake one here. */
    val push: PushChannel = UnifiedPushChannel(context),
    pushStore: EndpointStore = PrefsEndpointStore(context, secrets),
) {
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    val hosts = HostRepository(File(context.filesDir, "hosts.json"), secrets)
    val pairing = dev.agentmesh.android.hosts.PairingFlow(hosts, pairingClients)
    val themes = ThemeStore(context)
    val prefs = DevicePrefs(context)
    val notifier = Notifier(context)

    /** Where to go once the UI is up: a tapped notification's agent. */
    val deepLink = MutableStateFlow<DeepLink?>(null)

    /** A pairing link opened from outside (an `agentmesh://pair` link): the pairing screen takes it, and asks before using it. */
    val pairLink = MutableStateFlow<String?>(null)

    /** The (host, agent) whose screen is on top while the app is resumed: its notifications aren't posted, the user is looking. */
    val viewing = MutableStateFlow<Pair<String, String>?>(null)

    private val _runtimes = MutableStateFlow<Map<String, HostRuntime>>(emptyMap())
    val runtimes: StateFlow<Map<String, HostRuntime>> get() = _runtimes

    /** Gives each host this device's push endpoint, while push is on. */
    val pushRegistrar = PushRegistrar(pushStore, runtimes, prefs.push, scope)

    init {
        // The host's `app.theme`: the first host (in list order) that has set one itself (followedTheme).
        scope.launch {
            _runtimes.flatMapLatest { rts ->
                if (rts.isEmpty()) flowOf(null)
                else combine(rts.values.map { it.state }) { states -> followedTheme(states.toList()) }
            }.collect { themes.setHostTheme(it) }
        }
        scope.launch {
            hosts.hosts.collect { list ->
                val old = _runtimes.value
                val next = LinkedHashMap<String, HostRuntime>()
                for (h in list) next[h.id] = old[h.id] ?: HostRuntime(h.id, hosts, newApi, scope).also {
                    watchNotices(it)
                    it.start()
                    // UnifiedPush asks an app to register on every start: the library sends the endpoint again
                    // only when it changed.
                    if (prefs.push.value) runCatching { push.register(h.id, h.name) }
                }
                for ((id, rt) in old) if (id !in next) {
                    rt.stop()
                    noticeJobs.remove(id)?.cancel()
                    // The host learns the endpoint is gone at its next push (404/410) and drops it.
                    runCatching { push.unregister(id) }
                    pushRegistrar.forget(id)
                }
                _runtimes.value = next
            }
        }
    }

    private val noticeJobs = HashMap<String, Job>()

    /** What happens on a host that the user may want to hear about, filtered by that host's own notify settings. */
    private fun watchNotices(rt: HostRuntime) {
        noticeJobs[rt.hostId] = scope.launch {
            rt.notices.collect { n -> if (notifies(rt.state.value, n.kind)) show(rt.hostId, n) }
        }
    }

    /** Posts a notice unless the user is looking at that agent now. */
    private fun show(hostId: String, n: Notice) {
        if (viewing.value == (hostId to n.agentId)) return
        val many = hosts.hosts.value.size > 1
        notifier.post(hostId, if (many) hosts.get(hostId)?.name else null, n)
    }

    /**
     * A push from host [hostId] (its UnifiedPush instance), decrypted. The host already applied its notify
     * settings; a push from a daemon other than the one this host is now is dropped, and so is any while push is off.
     */
    fun onPush(hostId: String, p: PushPayload) {
        // A push the server held while the phone was offline can arrive after push was turned off.
        if (!prefs.push.value) return
        val host = hosts.get(hostId) ?: return
        if (host.daemonId != null && p.daemon != host.daemonId) return
        if (p.isTest) return notifier.showInfo(hostId, p.title, p.body)
        p.toNotice()?.let { show(hostId, it) }
    }

    /** Turns push on: picks the distributor (asking the user when there are several), then registers every host. */
    fun enablePush(activity: android.app.Activity, done: (Boolean) -> Unit) = push.pick(activity) { ok ->
        if (ok) {
            prefs.setPush(true)
            for (h in hosts.hosts.value) runCatching { push.register(h.id, h.name) }
        }
        done(ok)
    }

    /**
     * Turns push off: the distributor drops the registrations at once, then each reachable host is told to stop (all
     * at the same time, in the background; a host registers again only once its call is answered or timed out).
     */
    fun disablePush() {
        prefs.setPush(false)
        val ids = hosts.hosts.value.map { it.id }
        for (id in ids) {
            runCatching { push.unregister(id) }
            pushRegistrar.forget(id)
        }
        pushRegistrar.unregisterAt(ids)
    }

    fun nudgeAll() = _runtimes.value.values.forEach { it.nudge() }

    fun watchProcess(context: Context) {
        ProcessLifecycleOwner.get().lifecycle.addObserver(object : DefaultLifecycleObserver {
            override fun onStart(owner: LifecycleOwner) = nudgeAll()
        })
        val cm = context.getSystemService(ConnectivityManager::class.java)
        cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) = nudgeAll()
        })
    }
}

open class MeshApp : Application() {
    lateinit var container: AppContainer
        private set

    /** What the app runs on; a test application replaces the Keystore and the network. */
    protected open fun createContainer(): AppContainer = AppContainer(this, KeystoreSecretBox())

    override fun onCreate() {
        super.onCreate()
        container = createContainer()
        container.watchProcess(this)
    }
}
