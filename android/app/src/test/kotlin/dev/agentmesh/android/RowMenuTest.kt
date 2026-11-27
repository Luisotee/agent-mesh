package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.assertIsEnabled
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performTouchInput
import androidx.compose.ui.test.longClick
import androidx.compose.ui.test.performScrollTo
import androidx.compose.ui.test.performTextInput
import androidx.compose.ui.test.performTextReplacement
import androidx.test.core.app.ApplicationProvider
import com.connectrpc.Code
import dev.agentmesh.android.derive.SpawnForm
import dev.agentmesh.android.derive.defaultRole
import dev.agentmesh.android.derive.defaultTier
import dev.agentmesh.android.derive.knownRepos
import dev.agentmesh.android.derive.slugOf
import dev.agentmesh.android.derive.spawnProblem
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AppNav
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.gen.agentmesh.v1.Actor
import dev.agentmesh.gen.agentmesh.v1.ActorClass
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.Project
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File


/** The drawer's long-press menus: what a run and an agent offer, and that each action that ends or renames asks first. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w900dp-h2000dp-xxhdpi")
class RowMenuTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val app get() = ApplicationProvider.getApplicationContext<Application>()
    private val container get() = (app as MeshApp).container
    private lateinit var api: RecordingApi

    private fun host(actor: ActorClass, vararg agents: dev.agentmesh.gen.agentmesh.v1.Agent): HostRuntime {
        val h = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val deadline = System.currentTimeMillis() + 5_000
        var rt: HostRuntime? = null
        while (rt == null && System.currentTimeMillis() < deadline) { rt = container.runtimes.value[h.id]; Thread.sleep(20) }
        rt!!
        while (rt.api() == null && System.currentTimeMillis() < deadline) Thread.sleep(20)
        rt.onState(ConnState.Ready(HelloResponse.newBuilder().setActor(Actor.newBuilder().setClass_(actor).setId("device-1")).build()))
        rt.onFrame(snapshot(listOf(run("r1", "Fix the typo")), agents.toList()))
        return rt
    }

    private fun show(rt: HostRuntime) = compose.setContent {
        CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AppNav() } }
    }

    private fun waitUntil(cond: () -> Boolean) = compose.waitUntil(5_000) { cond() }


    private val lead = agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead")
    private val worker = agent("w1", parent = "lead", name = "shop-fix").toBuilder().setTerminalId("t1").setWorktree("/wt/fix").build()

    private fun setUp(actor: ActorClass = ActorClass.ACTOR_CLASS_DEVICE): HostRuntime {
        api = RecordingApi(); NotificationsTest.api = api
        val rt = host(actor, lead, worker)
        show(rt)
        return rt
    }

    @Test fun `a held run offers add, rename and stop, and its lead what an agent offers`() {
        setUp()
        compose.onNodeWithTag("lead-r1").performTouchInput { longClick() }
        for (t in listOf("menu-add-agent", "menu-rename-run", "menu-stop-run", "menu-open", "menu-terminal", "menu-stop-agent", "menu-retire-agent", "menu-copy-worktree")) compose.onNodeWithTag(t).assertExists()
    }

    @Test fun `Stop the run asks first, then sends StopRun`() {
        setUp()
        compose.onNodeWithTag("lead-r1").performTouchInput { longClick() }
        compose.onNodeWithTag("menu-stop-run").performClick()
        compose.onNodeWithText("Stop Fix the typo?").assertIsDisplayed()
        compose.onNodeWithTag("action-cancel").performClick()
        assertEquals(emptyList<String>(), api.lifecycle.toList())
        compose.onNodeWithTag("lead-r1").performTouchInput { longClick() }
        compose.onNodeWithTag("menu-stop-run").performClick()
        compose.onNodeWithTag("action-confirm").performClick()
        waitUntil { api.lifecycle.isNotEmpty() }
        assertEquals(listOf("stop-run r1"), api.lifecycle.toList())
    }

    @Test fun `Rename needs a new title, and sends it on confirm`() {
        setUp()
        compose.onNodeWithTag("lead-r1").performTouchInput { longClick() }
        compose.onNodeWithTag("menu-rename-run").performClick()
        // The title it has is not a change.
        compose.onNodeWithTag("action-confirm").assertIsNotEnabled()
        compose.onNodeWithTag("action-title").performTextReplacement("Fix every typo")
        compose.onNodeWithTag("action-confirm").performClick()
        waitUntil { api.lifecycle.isNotEmpty() }
        assertEquals(listOf("rename r1 Fix every typo"), api.lifecycle.toList())
    }

    @Test fun `a worker's menu stops or retires it after asking`() {
        setUp()
        compose.onNodeWithTag("worker-w1").performTouchInput { longClick() }
        compose.onNodeWithTag("menu-retire-agent").performClick()
        compose.onNodeWithText("Retire shop-fix?").assertIsDisplayed()
        compose.onNodeWithTag("action-confirm").performClick()
        waitUntil { api.lifecycle.isNotEmpty() }
        assertEquals(listOf("retire w1"), api.lifecycle.toList())
    }

    @Test fun `a read-only device's menu has only Open and Copy`() {
        setUp(ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE)
        compose.onNodeWithTag("worker-w1").performTouchInput { longClick() }
        compose.onNodeWithTag("menu-open").assertIsDisplayed()
        compose.onNodeWithTag("menu-copy-worktree").assertIsDisplayed()
        for (t in listOf("menu-terminal", "menu-stop-agent", "menu-retire-agent")) compose.onAllNodesWithTag(t).assertCountEquals(0)
    }

    @Test fun `a refusal is shown in the dialog, which stays open`() {
        setUp()
        api.failLifecycle = ApiException(Code.FAILED_PRECONDITION, "[failed_precondition] the run still has a worker")
        compose.onNodeWithTag("lead-r1").performTouchInput { longClick() }
        compose.onNodeWithTag("menu-stop-run").performClick()
        compose.onNodeWithTag("action-confirm").performClick()
        waitUntil { compose.onAllNodesWithTag("action-error").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("the run still has a worker").assertIsDisplayed()
        compose.onNodeWithTag("action-confirm").assertIsEnabled()
    }
}
