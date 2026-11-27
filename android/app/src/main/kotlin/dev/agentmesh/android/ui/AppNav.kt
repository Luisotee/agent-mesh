package dev.agentmesh.android.ui

import androidx.compose.animation.core.tween
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.ExitTransition
import androidx.compose.animation.EnterTransition
import androidx.compose.material3.Text
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.DrawerValue
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ModalDrawerSheet
import androidx.compose.material3.ModalNavigationDrawer
import androidx.compose.material3.PermanentDrawerSheet
import androidx.compose.material3.PermanentNavigationDrawer
import androidx.compose.material3.rememberDrawerState
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.LocalWindowInfo
import androidx.compose.ui.unit.dp
import androidx.navigation.compose.currentBackStackEntryAsState
import kotlinx.coroutines.launch
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
    /** Home: what waits on the user and the live runs. The drawer holds the projects. */
    const val HOME = "home"
    /** Settings › Hosts. */
    const val HOSTS = "hosts"
    const val ADD = "host/new"
    const val PAIR = "pair"
    const val EDIT = "host/{id}/edit"
    const val LOG = "host/{id}/log"
    const val HOST_SETTINGS = "host/{id}/settings"
    const val DOCK_ALL = "live"
    const val USAGE = "host/{id}/usage"
    const val AGENT = "host/{id}/agent/{agent}?tab={tab}"
    const val SETTINGS = "settings"

    fun edit(id: String) = "host/$id/edit"
    fun log(id: String) = "host/$id/log"
    fun hostSettings(id: String) = "host/$id/settings"
    fun usage(id: String) = "host/$id/usage"
    fun agent(id: String, agent: String, tab: Tab = Tab.Overview) = "host/$id/agent/$agent?tab=${tab.name}"
}

/** Below this width the drawer slides over the screen; from it, it stays open beside it. */
const val WIDE_DP = 840

/**
 * The app's screens, inside one drawer (the projects, what waits on the user, the quota): it slides over home and the
 * agent at a phone's width and stays open beside them from [WIDE_DP]. Opening an agent replaces the one on screen
 * (`popUpTo` home), so Back from any agent goes home; a notification's agent opens the same way.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AppNav(nav: NavHostController = rememberNavController()) {
    val container = LocalContainer.current
    val runtimes by container.runtimes.collectAsStateWithLifecycleCompat()
    fun openAgent(host: String, agent: String, tab: Tab) {
        nav.navigate(Routes.agent(host, agent, tab)) { popUpTo(Routes.HOME) }
    }
    // A tapped notification: home, then the agent on top of it, so Back goes home.
    val link by container.deepLink.collectAsStateWithLifecycleCompat()
    LaunchedEffect(link) {
        val l = link ?: return@LaunchedEffect
        // On a cold start the host's runtime isn't built yet: wait for it (a removed host never comes).
        val there = kotlinx.coroutines.withTimeoutOrNull(5_000) { container.runtimes.first { it.containsKey(l.hostId) } }
        container.deepLink.value = null
        if (there == null) return@LaunchedEffect
        // `popUpTo` drops whatever is open (an agent already on screen included), so the stack is always home, the agent,
        // however often a notification is tapped (NavTest).
        openAgent(l.hostId, l.agentId, l.tab)
    }
    // An agentmesh://pair link opened from outside: the pairing screen, with the link in its field (nothing is sent until Pair).
    val pairLink by container.pairLink.collectAsStateWithLifecycleCompat()
    LaunchedEffect(pairLink) {
        if (pairLink != null && nav.currentDestination?.route != Routes.PAIR) nav.navigate(Routes.PAIR) { launchSingleTop = true }
    }
    val entry by nav.currentBackStackEntryAsState()
    val route = entry?.destination?.route
    val current = if (route == Routes.AGENT) entry?.arguments?.let { it.getString("id").orEmpty() to it.getString("agent").orEmpty() } else null
    // The drawer is on the screens that are about the work; settings and forms use the whole window.
    val hasDrawer = route == Routes.HOME || route == Routes.AGENT || route == Routes.DOCK_ALL || route == Routes.USAGE
    val wide = with(LocalDensity.current) { LocalWindowInfo.current.containerSize.width.toDp() } >= WIDE_DP.dp
    val drawer = rememberDrawerState(DrawerValue.Closed)
    val scope = rememberCoroutineScope()
    val model = rememberSidebar()
    // What the sheet starts: `host`, `repo` and `run` (empty for a new run) joined by newlines (saved across rotation).
    var spawn by rememberSaveable { mutableStateOf<String?>(null) }
    var adding by rememberSaveable { mutableStateOf<String?>(null) }
    fun closeDrawer(then: () -> Unit) { scope.launch { drawer.close() }; then() }
    val actions = SidebarActions(
        openAgent = { h, a, t -> closeDrawer { openAgent(h, a, t) } },
        home = { closeDrawer { nav.popBackStack(Routes.HOME, inclusive = false) } },
        settings = { closeDrawer { nav.navigate(Routes.SETTINGS) } },
        live = { closeDrawer { nav.navigate(Routes.DOCK_ALL) { launchSingleTop = true } } },
        usage = { h -> closeDrawer { nav.navigate(Routes.usage(h)) } },
        newLead = { h, repo -> closeDrawer { spawn = "$h\n$repo\n" } },
        addAgent = { h, run -> closeDrawer { spawn = "$h\n\n$run" } },
        addProject = { h -> closeDrawer { adding = h } },
    )
    val sheet: @Composable () -> Unit = { SidebarContent(model, current, actions, Modifier.statusBarsPadding()) }
    // One scrim dim and a slide; no alpha over the window. The menu button of a screen opens it.
    val openDrawer: (() -> Unit)? = if (wide || !hasDrawer) null else ({ scope.launch { drawer.open() }; Unit })
    val content: @Composable () -> Unit = { Routed(nav, runtimes, model, openDrawer, ::openAgent, wide) }
    if (wide) {
        PermanentNavigationDrawer(drawerContent = { if (hasDrawer) PermanentDrawerSheet(Modifier.width(320.dp)) { sheet() } }, content = content)
    } else {
        // Swipe opens it from home only: elsewhere a horizontal drag belongs to the screen (a terminal, a code block).
        ModalNavigationDrawer(drawerContent = { ModalDrawerSheet { sheet() } }, drawerState = drawer, gesturesEnabled = route == Routes.HOME || drawer.isOpen, content = content)
    }
    spawn?.split('\n')?.let { (host, repo, run) -> runtimes[host]?.let { rt -> SpawnSheet(rt, runId = run.ifEmpty { null }, onDismiss = { spawn = null }, repo = repo) } }
    adding?.let { host -> runtimes[host]?.let { rt -> AddProjectSheet(rt) { adding = null } } }
}

@Composable
private fun Routed(
    nav: NavHostController, runtimes: Map<String, dev.agentmesh.android.HostRuntime>, model: SidebarModel, openDrawer: (() -> Unit)?,
    openAgent: (String, String, Tab) -> Unit, wide: Boolean,
) {
    val container = LocalContainer.current
    // One short slide (the old page gives way by a quarter of the width), no crossfade: an alpha over the whole window costs
    // a full-window layer on the first frame of every push (happy measured 10-18 ms). Still under reduced motion.
    val reduced = LocalReducedMotion.current
    val ms = 280
    val ease = androidx.compose.animation.core.FastOutSlowInEasing
    var newLead by rememberSaveable { mutableStateOf<String?>(null) }
    NavHost(
        nav, startDestination = Routes.HOME,
        enterTransition = { if (reduced) EnterTransition.None else slideInHorizontally(tween(ms, easing = ease)) { it } },
        exitTransition = { if (reduced) ExitTransition.None else slideOutHorizontally(tween(ms, easing = ease)) { -it / 4 } },
        popEnterTransition = { if (reduced) EnterTransition.None else slideInHorizontally(tween(ms, easing = ease)) { -it / 4 } },
        popExitTransition = { if (reduced) ExitTransition.None else slideOutHorizontally(tween(ms, easing = ease)) { it } },
    ) {
        composable(Routes.HOME) {
            HomeScreen(
                model, openDrawer, onAgent = openAgent, onLive = { nav.navigate(Routes.DOCK_ALL) }, onNewLead = { newLead = it },
                onPair = { nav.navigate(Routes.PAIR) }, onAdd = { nav.navigate(Routes.ADD) },
            )
        }
        composable(Routes.HOSTS) {
            HostsScreen(
                onBack = { nav.popBackStack() }, onAdd = { nav.navigate(Routes.ADD) }, onPair = { nav.navigate(Routes.PAIR) }, onEdit = { nav.navigate(Routes.edit(it)) },
                onHostSettings = { nav.navigate(Routes.hostSettings(it)) }, onUsage = { nav.navigate(Routes.usage(it)) }, onLog = { nav.navigate(Routes.log(it)) },
            )
        }
        composable(Routes.PAIR) {
            // The screen takes a waiting link (and any that arrives while it is up) itself, once.
            PairScreen(
                null, onBack = { nav.popBackStack() },
                onPaired = { nav.popBackStack(Routes.HOME, inclusive = false) },
            )
        }
        composable(Routes.ADD) { HostFormScreen(null) { nav.popBackStack() } }
        composable(Routes.EDIT) { HostFormScreen(it.arguments?.getString("id")) { nav.popBackStack() } }
        composable(Routes.SETTINGS) { DeviceSettingsScreen(onBack = { nav.popBackStack() }, onHosts = { nav.navigate(Routes.HOSTS) }) }
        composable(Routes.LOG) { e -> runtimes[e.arguments!!.getString("id")!!]?.let { LogScreen(it) { nav.popBackStack() } } }
        composable(Routes.DOCK_ALL) { AllHostsDockScreen({ nav.popBackStack() }) { host, agent, tab -> nav.navigate(Routes.agent(host, agent, tab)) } }
        composable(Routes.USAGE) { e -> runtimes[e.arguments!!.getString("id")!!]?.let { UsageScreen(it, onBack = { nav.popBackStack() }, onLog = { nav.navigate(Routes.log(e.arguments!!.getString("id")!!)) }) } ?: Gone { nav.popBackStack() } }
        composable(Routes.HOST_SETTINGS) { e -> runtimes[e.arguments!!.getString("id")!!]?.let { HostSettingsScreen(it) { nav.popBackStack() } } ?: Gone { nav.popBackStack() } }
        composable(
            Routes.AGENT,
            arguments = listOf(
                navArgument("tab") { type = NavType.StringType; defaultValue = Tab.Overview.name },
            ),
        ) { e ->
            val tab = runCatching { Tab.valueOf(e.arguments?.getString("tab").orEmpty()) }.getOrDefault(Tab.Overview)
            // A host removed while this was open (or a tile that was tapped as it went) has nothing to show.
            runtimes[e.arguments!!.getString("id")!!]?.let { AgentScreen(it, e.arguments!!.getString("agent")!!, { nav.popBackStack() }, tab, // Opened from Live, the agent keeps its Back arrow to it: the menu is for an agent opened from home or the drawer.
                onMenu = openDrawer.takeIf { nav.previousBackStackEntry?.destination?.route == Routes.HOME }) } ?: Gone { nav.popBackStack() }
        }
    }
    // The new-lead sheet of Home's button (the drawer's own is at the app's root).
    newLead?.let { host -> runtimes[host]?.let { rt -> SpawnSheet(rt, runId = null, onDismiss = { newLead = null }) } }
}

@Composable
private fun Gone(onBack: () -> Unit) {
    androidx.compose.runtime.LaunchedEffect(Unit) { onBack() }
    Text("This host was removed.")
}
