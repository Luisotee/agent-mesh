package dev.agentmesh.android.ui

import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import kotlinx.coroutines.flow.first
import androidx.compose.runtime.getValue
import androidx.navigation.NavHostController
import androidx.navigation.NavType
import androidx.navigation.compose.NavHost
import androidx.navigation.navArgument
import dev.agentmesh.android.notify.Tab
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import dev.agentmesh.android.ui.settings.DeviceSettingsScreen
import dev.agentmesh.android.ui.settings.HostSettingsScreen

object Routes {
    const val HOSTS = "hosts"
    const val ADD = "host/new"
    const val PAIR = "pair"
    const val EDIT = "host/{id}/edit"
    const val HOST = "host/{id}"
    const val LOG = "host/{id}/log"
    const val HOST_SETTINGS = "host/{id}/settings"
    const val DOCK = "host/{id}/live"
    const val USAGE = "host/{id}/usage"
    const val AGENT = "host/{id}/agent/{agent}?tab={tab}"
    const val SETTINGS = "settings"

    fun host(id: String) = "host/$id"
    fun edit(id: String) = "host/$id/edit"
    fun log(id: String) = "host/$id/log"
    fun hostSettings(id: String) = "host/$id/settings"
    fun dock(id: String) = "host/$id/live"
    fun usage(id: String) = "host/$id/usage"
    fun agent(id: String, agent: String, tab: Tab = Tab.Overview) = "host/$id/agent/$agent?tab=${tab.name}"
}

/** The app's screens. A deep link from a notification opens [Routes.agent]. */
@Composable
fun AppNav(nav: NavHostController = rememberNavController()) {
    val container = LocalContainer.current
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    // A tapped notification: the host, then the agent on top of it, so Back goes to the host.
    val link by container.deepLink.collectAsStateWithLifecycleCompat()
    LaunchedEffect(link) {
        val l = link ?: return@LaunchedEffect
        // On a cold start the host's runtime isn't built yet: wait for it (a removed host never comes).
        val there = kotlinx.coroutines.withTimeoutOrNull(5_000) { container.runtimes.first { it.containsKey(l.hostId) } }
        container.deepLink.value = null
        if (there == null) return@LaunchedEffect
        // `popUpTo` drops whatever is open (an agent already on screen included), so the stack is always hosts, the
        // host, the agent, however often a notification is tapped (NavTest).
        nav.navigate(Routes.host(l.hostId)) { popUpTo(Routes.HOSTS) }
        nav.navigate(Routes.agent(l.hostId, l.agentId, l.tab))
    }
    // An agentmesh://pair link opened from outside: the pairing screen, with the link in its field (nothing is sent until Pair).
    val pairLink by container.pairLink.collectAsStateWithLifecycleCompat()
    LaunchedEffect(pairLink) {
        if (pairLink != null && nav.currentDestination?.route != Routes.PAIR) nav.navigate(Routes.PAIR) { launchSingleTop = true }
    }
    NavHost(nav, startDestination = Routes.HOSTS) {
        composable(Routes.HOSTS) {
            HostsScreen(
                onOpen = { nav.navigate(Routes.host(it)) }, onAdd = { nav.navigate(Routes.ADD) }, onPair = { nav.navigate(Routes.PAIR) },
                onEdit = { nav.navigate(Routes.edit(it)) }, onSettings = { nav.navigate(Routes.SETTINGS) },
            )
        }
        composable(Routes.PAIR) {
            // The screen takes a waiting link (and any that arrives while it is up) itself, once.
            PairScreen(
                null, onBack = { nav.popBackStack() },
                onPaired = { id -> nav.navigate(Routes.host(id)) { popUpTo(Routes.HOSTS) } },
            )
        }
        composable(Routes.ADD) { HostFormScreen(null) { nav.popBackStack() } }
        composable(Routes.EDIT) { HostFormScreen(it.arguments?.getString("id")) { nav.popBackStack() } }
        composable(Routes.SETTINGS) { DeviceSettingsScreen { nav.popBackStack() } }
        composable(Routes.HOST) { e ->
            val id = e.arguments!!.getString("id")!!
            val rt = runtimes[id]
            if (rt == null) Gone { nav.popBackStack() } else HostScreen(
                rt, onBack = { nav.popBackStack() }, onAgent = { agent, tab -> nav.navigate(Routes.agent(id, agent, tab)) },
                onLog = { nav.navigate(Routes.log(id)) }, onSettings = { nav.navigate(Routes.hostSettings(id)) },
                onDock = { nav.navigate(Routes.dock(id)) }, onUsage = { nav.navigate(Routes.usage(id)) },
            )
        }
        composable(Routes.LOG) { e -> runtimes[e.arguments!!.getString("id")!!]?.let { LogScreen(it) { nav.popBackStack() } } }
        composable(Routes.DOCK) { e ->
            val id = e.arguments!!.getString("id")!!
            runtimes[id]?.let { DockScreen(it, { nav.popBackStack() }, onLog = { nav.navigate(Routes.log(id)) }) { agent, tab -> nav.navigate(Routes.agent(id, agent, tab)) } }
        }
        composable(Routes.USAGE) { e -> runtimes[e.arguments!!.getString("id")!!]?.let { UsageScreen(it, onBack = { nav.popBackStack() }, onLog = { nav.navigate(Routes.log(e.arguments!!.getString("id")!!)) }) } }
        composable(Routes.HOST_SETTINGS) { e -> runtimes[e.arguments!!.getString("id")!!]?.let { HostSettingsScreen(it) { nav.popBackStack() } } }
        composable(
            Routes.AGENT,
            arguments = listOf(
                navArgument("tab") { type = NavType.StringType; defaultValue = Tab.Overview.name },
            ),
        ) { e ->
            val tab = runCatching { Tab.valueOf(e.arguments?.getString("tab").orEmpty()) }.getOrDefault(Tab.Overview)
            runtimes[e.arguments!!.getString("id")!!]?.let { AgentScreen(it, e.arguments!!.getString("agent")!!, { nav.popBackStack() }, tab) }
        }
    }
}

@Composable
private fun Gone(onBack: () -> Unit) {
    androidx.compose.runtime.LaunchedEffect(Unit) { onBack() }
    Text("This host was removed.")
}
