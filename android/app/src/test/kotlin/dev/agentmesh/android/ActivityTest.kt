package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.derive.waitingAllFor
import dev.agentmesh.android.derive.waitingFor
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.store.MAX_ACTIVITY
import dev.agentmesh.android.store.applyFrame
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.ActivityRow
import dev.agentmesh.android.ui.ActivityScreen
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.activityRows
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.WaitingKind
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** The host's UserNotice events kept as the Activity list, what the screen does with them, and what waits on the user by `for_user`. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w411dp-h900dp-xxhdpi")
class ActivityTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val app get() = ApplicationProvider.getApplicationContext<Application>()
    private val container get() = (app as MeshApp).container

    @Test fun `every UserNotice is kept, for the user or not, oldest first and at most the last 200, and a snapshot keeps them`() {
        var s = HostState()
        s = applyFrame(s, userNotice(1, "plan", forUser = true, title = "a", at = 1_000))
        s = applyFrame(s, userNotice(2, "plan", forUser = false, title = "b", at = 2_000))
        assertEquals(listOf("a", "b"), s.activity.map { it.notice.title })
        assertEquals(listOf(1_000L, 2_000L), s.activity.map { it.atMs })
        assertEquals(listOf(1L, 2L), s.activity.map { it.seq })
        s = applyFrame(s, snapshot(emptyList(), emptyList()))
        assertEquals(2, s.activity.size)
        for (i in 3L..(MAX_ACTIVITY + 10L)) s = applyFrame(s, userNotice(i, "report", title = "n$i"))
        assertEquals(MAX_ACTIVITY, s.activity.size)
        assertEquals("n${MAX_ACTIVITY + 10}", s.activity.last().notice.title)
    }

    @Test fun `the list is newest first across hosts`() {
        val a = ActivityRow("h1", "wsl", dev.agentmesh.android.store.ActivityItem(1, 1_000, userNotice(1, "plan").event.userNotice))
        val b = ActivityRow("h2", "vps", dev.agentmesh.android.store.ActivityItem(1, 3_000, userNotice(1, "report").event.userNotice))
        val c = ActivityRow("h1", "wsl", dev.agentmesh.android.store.ActivityItem(2, 3_000, userNotice(2, "failed").event.userNotice))
        assertEquals(listOf(c, b, a), activityRows(listOf(a, b, c)))
    }

    @Test fun `the screen lists what the host said, marks the entries for the user, opens the agent, and counts them as read`() {
        val api = RecordingApi()
        NotificationsTest.api = api
        val host = container.hosts.add("wsl", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val deadline = System.currentTimeMillis() + 5_000
        var rt: HostRuntime? = null
        while (rt == null && System.currentTimeMillis() < deadline) { rt = container.runtimes.value[host.id]; Thread.sleep(20) }
        rt!!.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1")), listOf(agent("lead"), agent("w1", parent = "lead", name = "shop-fix"))))
        rt.onFrame(userNotice(5, "plan", title = "shop-fix sent a plan", body = "Change the typo", forUser = true, at = 2_000))
        rt.onFrame(userNotice(6, "report", agentId = "lead", title = "shop-fix reported done", body = "", forUser = false, tab = "terminal", at = 3_000))
        val opened = mutableListOf<Triple<String, String, Tab>>()
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { ActivityScreen({}, { h, a, t -> opened += Triple(h, a, t) }) } } }
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("activity-row").fetchSemanticsNodes().size == 2 }
        // Newest first; the one for the user says so.
        compose.onAllNodesWithText("shop-fix reported done").assertCountEquals(1)
        compose.onAllNodesWithText("Plan · for you", substring = true).assertCountEquals(1)
        compose.onAllNodesWithText("Report", substring = true).assertCountEquals(1)
        compose.onAllNodesWithTag("activity-row")[1].performClick()
        assertEquals(listOf(Triple(host.id, "w1", Tab.Chat)), opened)
        compose.onAllNodesWithTag("activity-row")[0].performClick()
        assertEquals(Tab.Terminal, opened.last().third)
        // Looking at the list is reading it.
        compose.waitUntil(5_000) { container.prefs.activitySeen.value[host.id] == 6L }
    }

    @Test fun `an empty list says so`() {
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { ActivityScreen({}, { _, _, _ -> }) } } }
        compose.onNodeWithTag("activity-empty").assertExists()
    }

    @Test fun `what waits on the user is only what the host says is for the user, and the run's own chat still has all of it`() {
        val plan = message("p1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "go", at = 10_000)
        val s = HostState(
            runs = mapOf("r1" to run("r1")), agents = listOf(agent("lead"), agent("w1", parent = "lead")).associateBy { it.id },
            waiting = listOf(waiting(WaitingKind.WAITING_KIND_PLAN, "w1", message = plan, forUser = false), waiting(WaitingKind.WAITING_KIND_INPUT, "lead", forUser = true)),
        )
        assertEquals(listOf("lead"), waitingFor(s).map { it.agent.id })
        assertEquals(listOf("w1", "lead"), waitingAllFor(s).map { it.agent.id })
    }
}
