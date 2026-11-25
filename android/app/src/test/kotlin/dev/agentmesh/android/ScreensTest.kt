package dev.agentmesh.android

import android.app.Application
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithContentDescription
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
import dev.agentmesh.android.ui.HostScreen
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
    override suspend fun openTerminal(agent: String): String = error("unused")
    override fun connectTerminal(path: String): dev.agentmesh.android.net.TerminalSocket = error("unused")
    override suspend fun listSettings(prefix: String) = emptyList<dev.agentmesh.gen.agentmesh.v1.Setting>()
    override suspend fun setSetting(key: String, value: com.google.protobuf.Value) = error("unused")
    override suspend fun resetSetting(key: String) = error("unused")
}

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h1600dp-xxhdpi")
class ScreensTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()

    private val theme = Themes.resolve(Themes.parse(File("src/main/assets/themes/dracula.json").readText()))

    private fun runtime(): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        return HostRuntime(host.id, repo, { _, _ -> Silent }, CoroutineScope(Dispatchers.Unconfined))
    }

    private fun HostRuntime.feed(vararg frames: dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse) {
        onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        frames.forEach(::onFrame)
    }

    @Test fun `the host screen shows each run as a tree and what waits on the user`() {
        val rt = runtime()
        val lead = agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead", state = AgentState.AGENT_STATE_WORKING)
        val worker = agent("w1", parent = "lead", name = "shop-fix-typo", state = AgentState.AGENT_STATE_NEEDS_INPUT, needsInput = 1, detail = "Bash", at = 2_000)
        rt.feed(snapshot(listOf(run("r1", "Fix the typo")), listOf(lead, worker)))
        rt.onFrame(messageSent(1, message("m1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, at = 3_000)))
        compose.setContent { AgentMeshTheme(theme) { HostScreen(rt, {}, { _, _ -> }, {}, {}) } }
        // The run's title heads its tree and sits under each waiting row.
        compose.onAllNodesWithText("Fix the typo").assertCountEquals(3)
        compose.onNodeWithText("shop-lead").assertIsDisplayed()
        compose.onNodeWithText("shop-fix-typo").assertIsDisplayed()
        compose.onNodeWithText("Needs you: Bash").assertIsDisplayed()
        // The plan and the prompt both wait on the user.
        compose.onNodeWithText("WAITING ON YOU (2)").assertIsDisplayed()
        compose.onNodeWithText("shop-fix-typo sent a plan").assertIsDisplayed()
    }

    @Test fun `a closed run is collapsed until it is tapped`() {
        val rt = runtime()
        rt.feed(snapshot(listOf(run("r1", "Old work", status = dev.agentmesh.gen.agentmesh.v1.RunStatus.RUN_STATUS_CLOSED)), listOf(agent("a1", name = "old-agent", state = AgentState.AGENT_STATE_EXITED))))
        compose.setContent { AgentMeshTheme(theme) { HostScreen(rt, {}, { _, _ -> }, {}, {}) } }
        compose.onNodeWithText("Old work").assertIsDisplayed()
        compose.onNodeWithText("old-agent").assertDoesNotExistCompat()
        compose.onNodeWithText("Old work").performClick()
        compose.onNodeWithText("old-agent").assertIsDisplayed()
    }

    @Test fun `a host that is not connected says why and offers a retry`() {
        val rt = runtime()
        rt.onState(ConnState.Stopped(ConnState.Reason.Auth, "bad token"))
        compose.setContent { AgentMeshTheme(theme) { HostScreen(rt, {}, { _, _ -> }, {}, {}) } }
        compose.onNodeWithText("The host refused this device's token").assertIsDisplayed()
        compose.onNodeWithText("Retry").assertIsDisplayed()
    }

    @Test fun `the agent screen shows where it works`() {
        val rt = runtime()
        rt.feed(snapshot(listOf(run("r1")), listOf(agent("a1", name = "shop-fix", state = AgentState.AGENT_STATE_WORKING).toBuilder().setBranch("mesh/r1/fix").setModel("opus").build())))
        val container = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> Silent })
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AgentScreen(rt, "a1", {}) } } }
        compose.onNodeWithText("Working").assertIsDisplayed()
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
