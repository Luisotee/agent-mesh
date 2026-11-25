package dev.agentmesh.android.ui

import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import dev.agentmesh.android.ui.settings.DeviceSettingsScreen
import dev.agentmesh.android.ui.settings.HostSettingsScreen

object Routes {
    const val HOSTS = "hosts"
    const val ADD = "host/new"
    const val EDIT = "host/{id}/edit"
    const val HOST = "host/{id}"
    const val LOG = "host/{id}/log"
    const val HOST_SETTINGS = "host/{id}/settings"
    const val AGENT = "host/{id}/agent/{agent}"
    const val SETTINGS = "settings"

    fun host(id: String) = "host/$id"
    fun edit(id: String) = "host/$id/edit"
    fun log(id: String) = "host/$id/log"
    fun hostSettings(id: String) = "host/$id/settings"
    fun agent(id: String, agent: String) = "host/$id/agent/$agent"
}

/** The app's screens. A deep link from a notification opens [Routes.agent]. */
@Composable
fun AppNav(nav: NavHostController = rememberNavController()) {
    val container = LocalContainer.current
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    NavHost(nav, startDestination = Routes.HOSTS) {
        composable(Routes.HOSTS) {
            HostsScreen(
                onOpen = { nav.navigate(Routes.host(it)) }, onAdd = { nav.navigate(Routes.ADD) },
                onEdit = { nav.navigate(Routes.edit(it)) }, onSettings = { nav.navigate(Routes.SETTINGS) },
            )
        }
        composable(Routes.ADD) { HostFormScreen(null) { nav.popBackStack() } }
        composable(Routes.EDIT) { HostFormScreen(it.arguments?.getString("id")) { nav.popBackStack() } }
        composable(Routes.SETTINGS) { DeviceSettingsScreen { nav.popBackStack() } }
        composable(Routes.HOST) { e ->
            val id = e.arguments!!.getString("id")!!
            val rt = runtimes[id]
            if (rt == null) Gone { nav.popBackStack() } else HostScreen(
                rt, onBack = { nav.popBackStack() }, onAgent = { nav.navigate(Routes.agent(id, it)) },
                onLog = { nav.navigate(Routes.log(id)) }, onSettings = { nav.navigate(Routes.hostSettings(id)) },
            )
        }
        composable(Routes.LOG) { e -> runtimes[e.arguments!!.getString("id")!!]?.let { LogScreen(it) { nav.popBackStack() } } }
        composable(Routes.HOST_SETTINGS) { e -> runtimes[e.arguments!!.getString("id")!!]?.let { HostSettingsScreen(it) { nav.popBackStack() } } }
        composable(Routes.AGENT) { e ->
            runtimes[e.arguments!!.getString("id")!!]?.let { AgentScreen(it, e.arguments!!.getString("agent")!!) { nav.popBackStack() } }
        }
    }
}

@Composable
private fun Gone(onBack: () -> Unit) {
    androidx.compose.runtime.LaunchedEffect(Unit) { onBack() }
    Text("This host was removed.")
}
