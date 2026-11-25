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
import dev.agentmesh.android.theme.ThemeStore
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import java.io.File

/**
 * What lives as long as the process: the host list and one [HostRuntime] per host, kept connected while the app
 * is in front (a foreground service for the background comes with notifications). The connection hints
 * (the app came to the foreground, a network came up) go to every host.
 */
class AppContainer(context: Context, secrets: dev.agentmesh.android.hosts.SecretBox, connector: RouteConnector = DefaultRouteConnector()) {
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    val hosts = HostRepository(File(context.filesDir, "hosts.json"), secrets)
    val themes = ThemeStore(context)

    private val _runtimes = MutableStateFlow<Map<String, HostRuntime>>(emptyMap())
    val runtimes: StateFlow<Map<String, HostRuntime>> get() = _runtimes

    private val newApi = { route: dev.agentmesh.android.net.Route, token: () -> String -> ConnectMeshApi(connector.connect(route, token)) }

    init {
        scope.launch {
            hosts.hosts.collect { list ->
                val old = _runtimes.value
                val next = LinkedHashMap<String, HostRuntime>()
                for (h in list) next[h.id] = old[h.id] ?: HostRuntime(h.id, hosts, newApi, scope).also { it.start() }
                for ((id, rt) in old) if (id !in next) rt.stop()
                _runtimes.value = next
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

class MeshApp : Application() {
    lateinit var container: AppContainer
        private set

    override fun onCreate() {
        super.onCreate()
        container = AppContainer(this, KeystoreSecretBox())
        container.watchProcess(this)
    }
}
