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
import dev.agentmesh.android.notify.NoPush
import dev.agentmesh.android.notify.Notifier
import dev.agentmesh.android.notify.PushChannel
import dev.agentmesh.android.notify.notifies
import dev.agentmesh.android.theme.ThemeStore
import dev.agentmesh.android.theme.hostTheme
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

    /** Push, when a channel is plugged in (UnifiedPush later); the live streams are the only channel today. */
    val push: PushChannel = NoPush

    private val _runtimes = MutableStateFlow<Map<String, HostRuntime>>(emptyMap())
    val runtimes: StateFlow<Map<String, HostRuntime>> get() = _runtimes

    init {
        // The host's `app.theme`: the first host (in list order) that has one set.
        scope.launch {
            _runtimes.flatMapLatest { rts ->
                if (rts.isEmpty()) flowOf(null)
                else combine(rts.values.map { it.state }) { states -> states.firstNotNullOfOrNull { hostTheme(it) } }
            }.collect { themes.setHostTheme(it) }
        }
        scope.launch {
            hosts.hosts.collect { list ->
                val old = _runtimes.value
                val next = LinkedHashMap<String, HostRuntime>()
                for (h in list) next[h.id] = old[h.id] ?: HostRuntime(h.id, hosts, newApi, scope).also { watchNotices(it); it.start() }
                for ((id, rt) in old) if (id !in next) { rt.stop(); noticeJobs.remove(id)?.cancel() }
                _runtimes.value = next
            }
        }
    }

    private val noticeJobs = HashMap<String, Job>()

    /** What happens on a host that the user may want to hear about, filtered by that host's own notify settings. */
    private fun watchNotices(rt: HostRuntime) {
        noticeJobs[rt.hostId] = scope.launch {
            rt.notices.collect { n ->
                if (!notifies(rt.state.value, n.kind)) return@collect
                if (viewing.value == (rt.hostId to n.agentId)) return@collect
                val many = hosts.hosts.value.size > 1
                notifier.post(rt.hostId, if (many) rt.host?.name else null, n)
            }
        }
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
