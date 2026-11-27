package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithContentDescription
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performTextInput
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.HomeScreen
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.SidebarActions
import dev.agentmesh.android.ui.SidebarContent
import dev.agentmesh.android.ui.rememberSidebar
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.AgyUsageRow
import dev.agentmesh.gen.agentmesh.v1.ClaudeUsage
import dev.agentmesh.gen.agentmesh.v1.ClaudeUsageWindow
import dev.agentmesh.gen.agentmesh.v1.GetUsageResponse
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import dev.agentmesh.gen.agentmesh.v1.WaitingKind
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** The drawer and home: projects across hosts, leads with their workers folded in, what waits on the user, the quota. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w411dp-h1600dp-xxhdpi")
class SidebarTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val container get() = (ApplicationProvider.getApplicationContext<Application>() as MeshApp).container
    private lateinit var api: RecordingApi

    private fun a(id: String, name: String, repo: String, run: String = "r1", role: AgentRole = AgentRole.AGENT_ROLE_WORKER, parent: String = "", state: AgentState = AgentState.AGENT_STATE_IDLE, at: Long = 1_000): Agent =
        agent(id, runId = run, role = role, parent = parent, state = state, name = name, at = at).toBuilder().setRepo(repo).build()

    private fun host(name: String, runs: List<dev.agentmesh.gen.agentmesh.v1.Run>, agents: List<Agent>, conn: ConnState = ConnState.Ready(HelloResponse.getDefaultInstance())): HostRuntime {
        val h = container.hosts.add(name, listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val deadline = System.currentTimeMillis() + 5_000
        var rt: HostRuntime? = null
        while (rt == null && System.currentTimeMillis() < deadline) { rt = container.runtimes.value[h.id]; Thread.sleep(20) }
        rt!!
        while (rt.api() == null && System.currentTimeMillis() < deadline) Thread.sleep(20)
        rt.onState(conn)
        if (conn is ConnState.Ready) rt.onFrame(snapshot(runs, agents))
        return rt
    }

    private val opened = mutableListOf<Triple<String, String, Tab>>()
    private val actions = SidebarActions(
        openAgent = { h, ag, t -> opened += Triple(h, ag, t) }, home = {}, settings = {}, live = {}, usage = { opened += Triple("usage", it, Tab.Overview) },
        newLead = { _, _ -> }, addAgent = { _, _ -> }, addProject = {},
    )

    private fun drawer(current: Pair<String, String>? = null) = compose.setContent {
        CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { SidebarContent(rememberSidebar(), current, actions) } }
    }

    private fun waitForText(t: String) = compose.waitUntil(5_000) { compose.onAllNodesWithText(t).fetchSemanticsNodes().isNotEmpty() }

    private val shop = "/home/u/shop-api"
    private val web = "/home/u/web"

    private val team = listOf(
        a("lead", "shop-lead", shop, role = AgentRole.AGENT_ROLE_ROOT, state = AgentState.AGENT_STATE_WORKING),
        a("w1", "shop-fix", shop, parent = "lead", state = AgentState.AGENT_STATE_NEEDS_INPUT, at = 2_000).toBuilder().setNeedsInput(1).setStateDetail("Bash").build(),
        a("rv1", "shop-review-1", shop, role = AgentRole.AGENT_ROLE_REVIEWER, parent = "w1", at = 3_000),
        a("rv2", "shop-review-2", shop, role = AgentRole.AGENT_ROLE_REVIEWER, parent = "w1", at = 4_000),
        a("lead2", "web-lead", web, run = "r2", role = AgentRole.AGENT_ROLE_ROOT),
    )
    private val runs = listOf(run("r1", "Fix the typo"), run("r2", "Docs"))

    @Test fun `the drawer lists projects with their leads and workers, and reviewers as one chip`() {
        api = RecordingApi(); NotificationsTest.api = api
        host("wsl", runs, team)
        drawer()
        waitForText("shop-api")
        compose.onNodeWithText("shop-api").assertIsDisplayed()
        compose.onNodeWithText("web").assertIsDisplayed()
        // A lead stands for its run; its worker is under it, and the reviewers are no rows.
        compose.onNodeWithTag("lead-r1").assertIsDisplayed()
        compose.onNodeWithTag("worker-w1").assertIsDisplayed()
        compose.onAllNodesWithText("shop-review-1").assertCountEquals(0)
        compose.onNodeWithText("·2 reviews").assertIsDisplayed()
        // One host: no host badge.
        compose.onAllNodesWithText("wsl").assertCountEquals(0)
        compose.onNodeWithTag("worker-w1").performClick()
        assertEquals(listOf(Triple(container.hosts.hosts.value.single().id, "w1", Tab.Chat)), opened)
    }

    @Test fun `what waits on the user is a section of its own, answered in place`() {
        api = RecordingApi(); NotificationsTest.api = api
        val plan = message("p1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "Change the typo", at = 10_000)
        api.waitingList = listOf(waiting(WaitingKind.WAITING_KIND_PLAN, "w1", message = plan))
        host("wsl", runs, team)
        drawer()
        waitForText("shop-fix sent a plan")
        compose.onNodeWithText("Waiting on you (1)").assertIsDisplayed()
        compose.onNodeWithText("shop-fix sent a plan").performClick()
        compose.onNodeWithText("Change the typo").assertIsDisplayed()
    }

    @Test fun `with nothing waiting there is no waiting section`() {
        api = RecordingApi(); NotificationsTest.api = api
        host("wsl", listOf(run("r2", "Docs")), listOf(a("lead2", "web-lead", web, run = "r2", role = AgentRole.AGENT_ROLE_ROOT)))
        drawer()
        waitForText("web")
        compose.onAllNodesWithTag("sidebar-waiting").assertCountEquals(0)
    }

    @Test fun `a finished run folds under Earlier until it is opened`() {
        api = RecordingApi(); NotificationsTest.api = api
        host(
            "wsl", listOf(run("r1", "Live work"), run("r2", "Old work", status = RunStatus.RUN_STATUS_CLOSED)),
            listOf(a("l1", "live-lead", shop, role = AgentRole.AGENT_ROLE_ROOT, state = AgentState.AGENT_STATE_WORKING), a("l2", "old-lead", shop, run = "r2", role = AgentRole.AGENT_ROLE_ROOT, state = AgentState.AGENT_STATE_EXITED)),
        )
        drawer()
        waitForText("Live work")
        compose.onAllNodesWithText("Old work").assertCountEquals(0)
        compose.onNodeWithTag("fold-Earlier").performClick()
        compose.onNodeWithText("Old work").assertIsDisplayed()
    }

    @Test fun `search narrows the drawer to what matches`() {
        api = RecordingApi(); NotificationsTest.api = api
        host("wsl", runs, team)
        drawer()
        waitForText("shop-api")
        compose.onNodeWithTag("sidebar-search").performTextInput("docs")
        compose.waitUntil(5_000) { compose.onAllNodesWithText("shop-api").fetchSemanticsNodes().isEmpty() }
        compose.onNodeWithText("Docs").assertIsDisplayed()
        compose.onNodeWithTag("sidebar-search").performTextInput(" nothing")
        waitForText("Nothing matches.")
    }

    @Test fun `a project is pinned or hidden from its menu, on this device only`() {
        api = RecordingApi(); NotificationsTest.api = api
        val rt = host("wsl", runs, team)
        drawer()
        waitForText("shop-api")
        compose.onNodeWithContentDescription("Menu of web").performClick()
        compose.onNodeWithText("Pin to the top").performClick()
        assertEquals(mapOf(rt.hostId to listOf(web)), container.prefs.pins.value)
        // Pinned, it leads the list.
        compose.waitUntil(5_000) { compose.onNodeWithTag("project-web").fetchSemanticsNode().positionInRoot.y < compose.onNodeWithTag("project-shop-api").fetchSemanticsNode().positionInRoot.y }
        // Hidden, it goes to the fold at the foot, and Show brings it back.
        compose.onNodeWithContentDescription("Menu of web").performClick()
        compose.onNodeWithText("Hide").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("project-web").fetchSemanticsNodes().isEmpty() }
        compose.onNodeWithTag("hidden-fold").performClick()
        compose.onNodeWithText("Show").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("project-web").fetchSemanticsNodes().isNotEmpty() }
        assertTrue(container.prefs.hidden.value.isEmpty())
    }

    @Test fun `a hidden project comes back while something in it waits on the user`() {
        api = RecordingApi(); NotificationsTest.api = api
        val rt = host("wsl", runs, team)
        container.prefs.toggleHidden(dev.agentmesh.android.derive.projectKey(rt.hostId, shop))
        drawer()
        // shop-api has the worker that needs the user: it stays.
        waitForText("shop-api")
        compose.onNodeWithText("shop-api").assertIsDisplayed()
    }

    @Test fun `with two hosts a project carries its host, and the same folder on both is two rows`() {
        api = RecordingApi(); NotificationsTest.api = api
        host("wsl", listOf(run("r1", "Fix the typo")), team.filter { it.runId == "r1" })
        host("vps", listOf(run("r1", "Fix the typo")), team.filter { it.runId == "r1" })
        drawer()
        waitForText("vps")
        compose.onAllNodesWithTag("project-shop-api").assertCountEquals(2)
        compose.onNodeWithText("wsl").assertIsDisplayed()
        compose.onNodeWithText("vps").assertIsDisplayed()
    }

    @Test fun `a host that is down says so in one line`() {
        api = RecordingApi(); NotificationsTest.api = api
        host("wsl", emptyList(), emptyList(), conn = ConnState.Reconnecting(2, System.currentTimeMillis() + 30_000, "connection refused"))
        drawer()
        waitForText("wsl: Offline: connection refused")
        compose.onNodeWithTag("host-offline").assertIsDisplayed()
    }

    @Test fun `the quota footer is a meter per window, amber at the reserve, and opens that host's usage`() {
        api = RecordingApi().apply {
            usage = GetUsageResponse.newBuilder().setQuotaStop(90).setMinQuota(10)
                .setClaude(
                    ClaudeUsage.newBuilder().addAtReserve("7 d")
                        .addWindows(ClaudeUsageWindow.newBuilder().setKey("5h").setLabel("5 h").setUsed(40.0))
                        .addWindows(ClaudeUsageWindow.newBuilder().setKey("7d").setLabel("7 d").setUsed(92.0)),
                )
                .addAgyRows(AgyUsageRow.newBuilder().setGroup("Gemini").setLabel("Pro").setLeft(60))
                .build()
        }
        NotificationsTest.api = api
        val rt = host("wsl", runs, team)
        drawer()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("quota-footer").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("Claude 5 h").assertIsDisplayed()
        compose.onNodeWithText("92%").assertIsDisplayed()
        compose.onNodeWithText("agy Gemini Pro").assertIsDisplayed()
        compose.onNodeWithTag("quota-wsl").performClick()
        assertEquals(listOf(Triple("usage", rt.hostId, Tab.Overview)), opened)
    }

    @Test fun `home shows each live run as its lead with its workers counted, and what waits`() {
        api = RecordingApi(); NotificationsTest.api = api
        host("wsl", runs, team)
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { HomeScreen(rememberSidebar(), null, { h, ag, t -> opened += Triple(h, ag, t) }, {}, {}, {}, {}) } }
        }
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("run-card").fetchSemanticsNodes().size == 2 }
        // The title heads the run's card and sits under its waiting row.
        compose.onAllNodesWithText("Fix the typo").assertCountEquals(2)
        compose.onNodeWithText("1 worker · 2 reviews").assertIsDisplayed()
        compose.onNodeWithText("Waiting on you (1)").assertIsDisplayed()
        compose.onAllNodesWithTag("run-card")[0].performClick()
        assertEquals("lead", opened.single().second)
    }

    @Test fun `home asks for a host when there is none`() {
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { HomeScreen(rememberSidebar(), null, { _, _, _ -> }, {}, {}, {}, {}) } }
        }
        compose.onNodeWithText("No hosts yet").assertIsDisplayed()
        compose.onNodeWithTag("pair-host").assertIsDisplayed()
        compose.onAllNodesWithTag("new-run").assertCountEquals(0)
    }

    @Test fun `pins and hides are kept on this device across a restart of the prefs`() {
        val c = container
        c.prefs.togglePin("h1", "/p/a"); c.prefs.togglePin("h1", "/p/b"); c.prefs.togglePin("h2", "/p/a"); c.prefs.toggleHidden("h1\n/p/c")
        val again = dev.agentmesh.android.DevicePrefs(ApplicationProvider.getApplicationContext<Application>())
        assertEquals(mapOf("h1" to listOf("/p/a", "/p/b"), "h2" to listOf("/p/a")), again.pins.value)
        assertEquals(setOf("h1\n/p/c"), again.hidden.value)
        again.togglePin("h1", "/p/a")
        assertEquals(listOf("/p/b"), again.pins.value["h1"])
        // A removed host's are forgotten.
        again.forgetHost("h1")
        assertEquals(mapOf("h2" to listOf("/p/a")), again.pins.value)
        assertTrue(again.hidden.value.isEmpty())
        assertEquals(mapOf("h2" to listOf("/p/a")), dev.agentmesh.android.DevicePrefs(ApplicationProvider.getApplicationContext<Application>()).pins.value)
    }

    @Test fun `a run with no agent can still be given one, from the drawer and from home`() {
        api = RecordingApi(); NotificationsTest.api = api
        host("wsl", listOf(run("r9", "Empty run")), emptyList())
        val added = mutableListOf<Pair<String, String>>()
        val acts = SidebarActions({ _, _, _ -> }, {}, {}, {}, {}, { _, _ -> }, { h, r -> added += h to r }, {})
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) {
                AgentMeshTheme(theme) {
                    val m = rememberSidebar()
                    androidx.compose.foundation.layout.Column {
                        androidx.compose.foundation.layout.Box(androidx.compose.ui.Modifier.weight(1f)) { SidebarContent(m, null, acts) }
                        androidx.compose.foundation.layout.Box(androidx.compose.ui.Modifier.weight(1f)) { HomeScreen(m, null, { _, _, _ -> }, {}, {}, {}, {}, onAddAgent = { h, r -> added += h to r }) }
                    }
                }
            }
        }
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("lead-r9").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("lead-r9").performClick()
        compose.onNodeWithText("Add an agent").performClick()
        compose.onNodeWithTag("run-card").performClick()
        assertEquals(2, added.size)
        assertEquals("r9", added[0].second)
    }

    @Test fun `a project that stays listed because it waits on you can be shown again, not hidden twice`() {
        api = RecordingApi(); NotificationsTest.api = api
        val rt = host("wsl", runs, team)
        container.prefs.toggleHidden(dev.agentmesh.android.derive.projectKey(rt.hostId, shop))
        drawer()
        waitForText("shop-api")
        compose.onNodeWithContentDescription("Menu of shop-api").performClick()
        compose.onNodeWithText("Show again").assertIsDisplayed()
    }
}
