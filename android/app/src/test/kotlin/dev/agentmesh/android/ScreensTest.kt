package dev.agentmesh.android

import android.app.Application
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithContentDescription
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.performClick
import androidx.compose.runtime.CompositionLocalProvider
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.hosts.SecretBox
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AgentScreen
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.HostsScreen
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.MessageType
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.emptyFlow
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** A host that never answers: the screens are driven by the state the test puts in. */
private object Silent : MeshApi {
    override suspend fun hello(): HelloResponse = kotlinx.coroutines.CompletableDeferred<HelloResponse>().await()
    override fun subscribe(cursor: dev.agentmesh.gen.agentmesh.v1.Cursor?): Flow<dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse> = emptyFlow()
    override suspend fun listMessages(runId: String, afterId: String): dev.agentmesh.gen.agentmesh.v1.ListMessagesResponse = error("unused")
    override suspend fun sendMessage(to: String, type: dev.agentmesh.gen.agentmesh.v1.MessageType, body: String): dev.agentmesh.gen.agentmesh.v1.Message = error("unused")
    override suspend fun approveAgent(agent: String): dev.agentmesh.gen.agentmesh.v1.Message = error("unused")
    override suspend fun openTerminal(agent: String, input: Boolean): String = error("unused")
    override fun connectTerminal(path: String): dev.agentmesh.android.net.TerminalSocket = error("unused")
    override suspend fun listSettings(prefix: String) = emptyList<dev.agentmesh.gen.agentmesh.v1.Setting>()
    override suspend fun setSetting(key: String, value: com.google.protobuf.Value) = error("unused")
    override suspend fun resetSetting(key: String) = error("unused")
    override suspend fun revokeSelf() = error("unused")
    override suspend fun registerPush(endpoint: String, p256dh: String, auth: String, events: List<String>): dev.agentmesh.gen.agentmesh.v1.PushRegistration = error("unused")
    override suspend fun unregisterPush(): Boolean = error("unused")
    override suspend fun testPush(): dev.agentmesh.gen.agentmesh.v1.PushRegistration = error("unused")
    override suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse = error("unused")
    override suspend fun promptAgent(agent: String, text: String) = error("unused")
    override suspend fun listPrompts(agent: String) = error("unused")
    override suspend fun withdrawPrompt(agent: String, promptId: String) = error("unused")
    override suspend fun interruptAgent(agent: String) = error("unused")
    override suspend fun switchAgent(agent: String, tier: String, model: String, effort: String): dev.agentmesh.gen.agentmesh.v1.SwitchAgentResponse = error("unused")
    override suspend fun checkModels(): dev.agentmesh.gen.agentmesh.v1.CheckModelsResponse = error("unused")
    override suspend fun listAgents(): List<dev.agentmesh.gen.agentmesh.v1.Agent> = error("unused")
    override suspend fun listWaiting(): List<dev.agentmesh.gen.agentmesh.v1.Waiting> = error("unused")
    override suspend fun listPullRequests(runId: String): List<dev.agentmesh.gen.agentmesh.v1.AgentPullRequests> = error("unused")
    override suspend fun holdPullRequest(url: String, release: Boolean): List<dev.agentmesh.gen.agentmesh.v1.PullRequest> = error("unused")
    override suspend fun getUsage(refresh: Boolean): dev.agentmesh.gen.agentmesh.v1.GetUsageResponse = error("unused")
    override suspend fun getCost(runId: String, since: String, agents: Boolean): dev.agentmesh.gen.agentmesh.v1.GetCostResponse = error("unused")
    override suspend fun startRun(title: String): dev.agentmesh.gen.agentmesh.v1.Run = error("unused")
    override suspend fun spawnAgent(runId: String, repo: String, slug: String, role: dev.agentmesh.gen.agentmesh.v1.AgentRole, kind: String, tier: String, model: String, task: String): dev.agentmesh.gen.agentmesh.v1.SpawnAgentResponse = error("unused")
    override suspend fun listProjects(): List<dev.agentmesh.gen.agentmesh.v1.Project> = error("unused")
    override suspend fun stopAgent(agent: String) = error("unused")
    override suspend fun retireAgent(agent: String) = error("unused")
    override suspend fun renameRun(runId: String, title: String) = error("unused")
    override suspend fun stopRun(runId: String): List<String> = error("unused")
}

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h1600dp-xxhdpi")
class ScreensTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()

    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))

    private fun runtime(): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        return HostRuntime(host.id, repo, { _, _ -> Silent }, CoroutineScope(Dispatchers.Unconfined))
    }

    private fun HostRuntime.feed(vararg frames: dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse) {
        onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        frames.forEach(::onFrame)
    }

    @Test fun `the agent screen shows where it works`() {
        val rt = runtime()
        rt.feed(snapshot(listOf(run("r1")), listOf(agent("a1", name = "shop-fix", state = AgentState.AGENT_STATE_WORKING).toBuilder().setBranch("mesh/r1/fix").setModel("opus").build())))
        val container = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> Silent })
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AgentScreen(rt, "a1", {}) } } }
        compose.onNodeWithText("Working").assertIsDisplayed()
        // Where it works is in the details (⋯), not on the chat's screen.
        compose.onNodeWithTag("details-open").performClick()
        compose.onNodeWithText("mesh/r1/fix").assertIsDisplayed()
        compose.onNodeWithText("shop-api").assertIsDisplayed()
    }

    @Test fun `the hosts screen starts empty and lists a host with its state`() {
        val ctx = ApplicationProvider.getApplicationContext<Application>()
        File(ctx.filesDir, "hosts.json").delete()
        val container = AppContainer(ctx, Plain)
        container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { HostsScreen({}, {}, {}, {}, {}) } }
        }
        compose.waitUntil(5_000) { compose.onAllNodesWithTextCompat("Home") }
        compose.onNodeWithText("Home").assertIsDisplayed()
        compose.onNodeWithContentDescription("Add a host").assertIsDisplayed()
    }
}

private fun androidx.compose.ui.test.SemanticsNodeInteraction.assertDoesNotExistCompat() = assertDoesNotExist()
private fun androidx.compose.ui.test.junit4.ComposeContentTestRule.onAllNodesWithTextCompat(t: String) = onAllNodesWithText(t).fetchSemanticsNodes().isNotEmpty()
