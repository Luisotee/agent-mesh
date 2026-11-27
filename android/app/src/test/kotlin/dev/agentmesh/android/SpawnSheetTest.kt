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

/** Starting a run and adding an agent from the phone: the form's rules (the desktop's), its calls, and who sees it. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w900dp-h2000dp-xxhdpi")
class SpawnSheetTest {
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

    @Test fun `the form's rules are the desktop's`() {
        assertEquals(AgentRole.AGENT_ROLE_ROOT, defaultRole(false)); assertEquals(AgentRole.AGENT_ROLE_WORKER, defaultRole(true))
        // A lead may take the CLI's default; a worker always has a tier: mid when the host has it, else its first.
        assertEquals("", defaultTier(AgentRole.AGENT_ROLE_ROOT, listOf("light", "mid")))
        assertEquals("mid", defaultTier(AgentRole.AGENT_ROLE_WORKER, listOf("light", "mid", "strong")))
        assertEquals("light", defaultTier(AgentRole.AGENT_ROLE_WORKER, listOf("light", "strong")))
        assertEquals("fix-the-typo-in-readme", slugOf("Fix the typo in README!"))
        assertEquals("", slugOf("???"))
        val ok = SpawnForm(repo = "/p/app", slug = "fix", role = AgentRole.AGENT_ROLE_WORKER, tier = "mid")
        assertNull(spawnProblem(ok))
        assertTrue(spawnProblem(ok.copy(repo = " "))!!.contains("repository"))
        assertTrue(spawnProblem(ok.copy(slug = ""))!!.contains("short name"))
        assertTrue(spawnProblem(ok.copy(slug = "Fix_it"))!!.contains("lower-case"))
        assertTrue(spawnProblem(ok.copy(slug = "a".repeat(41)))!!.contains("40"))
        assertNull(spawnProblem(ok.copy(slug = "a".repeat(40))))
        assertTrue(spawnProblem(ok.copy(kind = ""))!!.contains("CLI"))
        assertTrue(spawnProblem(ok.copy(tier = ""))!!.contains("tier or a model"))
        assertNull(spawnProblem(ok.copy(tier = "", model = "opus")))
        assertNull(spawnProblem(ok.copy(role = AgentRole.AGENT_ROLE_ROOT, tier = "")))
        assertEquals(listOf("/p/a", "/p/b"), knownRepos(listOf(agent("x").toBuilder().setRepo("/p/a").build(), agent("y").toBuilder().setRepo("/p/b").build(), agent("z").toBuilder().setRepo("/p/a").build(), agent("w").toBuilder().setRepo("").build())))
    }

    @Test fun `a read-only device is offered neither a new run nor an added agent`() {
        api = RecordingApi(); NotificationsTest.api = api
        val rt = host(ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE, agent("lead", role = AgentRole.AGENT_ROLE_ROOT))
        show(rt)
        compose.onAllNodesWithTag("new-run").assertCountEquals(0)
        compose.onNodeWithTag("lead-r1").performTouchInput { longClick() }
        compose.onNodeWithText("Add an agent").assertIsNotEnabled()
        // The run is in the drawer (open at this width) and on home.
        compose.onAllNodesWithText("Fix the typo")[0].assertIsDisplayed()
    }

    @Test fun `a new run starts with its lead, from the title and a repo of the host, with the defaults of a lead`() {
        api = RecordingApi().apply { projects = listOf(Project.newBuilder().setPath("/home/u/shop-api").setName("shop-api").build()) }
        NotificationsTest.api = api
        val rt = host(ActorClass.ACTOR_CLASS_DEVICE)
        show(rt)
        compose.onNodeWithTag("new-run").performClick()
        compose.onNodeWithTag("spawn-go").assertIsNotEnabled()
        compose.onNodeWithTag("run-title").performTextInput("Fix the typo")
        compose.onNodeWithTag("spawn-problem").performScrollTo().assertIsDisplayed()
        waitUntil { compose.onAllNodesWithTag("repo-/home/u/shop-api").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("repo-/home/u/shop-api").performScrollTo().performClick()
        // The name comes from the title until the user writes one.
        compose.onNodeWithTag("task").performScrollTo().performTextInput("fix it")
        compose.onNodeWithTag("spawn-go").performScrollTo().assertIsEnabled().performClick()
        waitUntil { api.spawns.isNotEmpty() }
        assertEquals(listOf("Fix the typo"), api.runsStarted.toList())
        // run, repo, slug, role, kind, tier, model, task
        assertEquals(listOf("run-1", "/home/u/shop-api", "fix-the-typo", "AGENT_ROLE_ROOT", "claude", "", "", "fix it"), api.spawns.single())
        // The sheet closes when it is done.
        waitUntil { compose.onAllNodesWithTag("spawn-sheet").fetchSemanticsNodes().isEmpty() }
    }

    @Test fun `a run alone can be started with no agent, and the host's refusal of a spawn is shown`() {
        api = RecordingApi(); NotificationsTest.api = api
        val rt = host(ActorClass.ACTOR_CLASS_DEVICE)
        show(rt)
        compose.onNodeWithTag("new-run").performClick()
        compose.onNodeWithTag("run-title").performTextInput("Empty run")
        compose.onNodeWithTag("with-agent").performClick()
        compose.onNodeWithTag("spawn-go").performScrollTo().assertIsEnabled().performClick()
        waitUntil { api.runsStarted.isNotEmpty() }
        assertEquals(emptyList<List<String>>(), api.spawns.toList())
        waitUntil { compose.onAllNodesWithTag("spawn-sheet").fetchSemanticsNodes().isEmpty() }
        // With an agent, a refusal says why and the sheet stays.
        api.failSpawn = ApiException(Code.RESOURCE_EXHAUSTED, "[resource_exhausted] 16 of 16 agents live (caps.agents)")
        compose.onNodeWithTag("new-run").performClick()
        compose.onNodeWithTag("run-title").performTextInput("Too many")
        compose.onNodeWithTag("repo").performScrollTo().performTextInput("/home/u/app")
        compose.onNodeWithTag("spawn-go").performScrollTo().performClick()
        waitUntil { compose.onAllNodesWithTag("spawn-error").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("16 of 16 agents live (caps.agents)").assertIsDisplayed()
        compose.onNodeWithTag("spawn-sheet").assertIsDisplayed()
    }

    @Test fun `an agent is added to a run as a worker once it has a lead, on mid, with its task`() {
        api = RecordingApi(); NotificationsTest.api = api
        val rt = host(ActorClass.ACTOR_CLASS_DEVICE, agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead"))
        show(rt)
        // Held down, the run in the (always open, at this width) drawer offers an agent.
        compose.onNodeWithTag("lead-r1").performTouchInput { longClick() }
        compose.onNodeWithText("Add an agent").performClick()
        compose.onNodeWithTag("repo").performScrollTo().performTextInput("/home/u/shop-api")
        compose.onNodeWithTag("slug").performScrollTo().performTextInput("Bad Name")
        compose.onNodeWithTag("spawn-problem").performScrollTo()
        compose.onNodeWithText("The name is lower-case", substring = true).assertIsDisplayed()
        compose.onNodeWithTag("spawn-go").assertIsNotEnabled()
        // Fix the name: the defaults are a worker on mid.
        compose.onNodeWithTag("slug").performScrollTo().performTextReplacement("fix-it")
        compose.onNodeWithTag("task").performScrollTo().performTextInput("take the ticket")
        compose.onNodeWithTag("spawn-go").performScrollTo().assertIsEnabled().performClick()
        waitUntil { api.spawns.isNotEmpty() }
        assertEquals(emptyList<String>(), api.runsStarted.toList())
        assertEquals(listOf("r1", "/home/u/shop-api", "fix-it", "AGENT_ROLE_WORKER", "claude", "mid", "", "take the ticket"), api.spawns.single())
    }

    @Test fun `offline the sheet says so and can't start anything`() {
        api = RecordingApi(); NotificationsTest.api = api
        val rt = host(ActorClass.ACTOR_CLASS_DEVICE)
        show(rt)
        compose.onNodeWithTag("new-run").performClick()
        compose.onNodeWithTag("run-title").performTextInput("Later")
        rt.onState(ConnState.Reconnecting(1, System.currentTimeMillis() + 60_000, "gone"))
        waitUntil { compose.onAllNodesWithTag("spawn-offline").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("spawn-go").performScrollTo().assertIsNotEnabled()
        assertEquals(emptyList<String>(), api.runsStarted.toList())
    }

    @Test fun `a retry after the agent was refused adds to the run the first press made, not another`() {
        api = RecordingApi().apply { failSpawn = ApiException(Code.RESOURCE_EXHAUSTED, "[resource_exhausted] no room for another agent") }
        NotificationsTest.api = api
        val rt = host(ActorClass.ACTOR_CLASS_DEVICE)
        show(rt)
        compose.onNodeWithTag("new-run").performClick()
        compose.onNodeWithTag("run-title").performTextInput("Once")
        compose.onNodeWithTag("repo").performScrollTo().performTextInput("/home/u/app")
        compose.onNodeWithTag("spawn-go").performScrollTo().performClick()
        waitUntil { compose.onAllNodesWithTag("spawn-error").fetchSemanticsNodes().isNotEmpty() }
        assertEquals(listOf("Once"), api.runsStarted.toList())
        // The host has room now: the second press spawns into the same run.
        api.failSpawn = null
        compose.onNodeWithTag("spawn-go").performScrollTo().performClick()
        waitUntil { api.spawns.isNotEmpty() }
        assertEquals(listOf("Once"), api.runsStarted.toList())
        assertEquals("run-1", api.spawns.single()[0])
    }
}
