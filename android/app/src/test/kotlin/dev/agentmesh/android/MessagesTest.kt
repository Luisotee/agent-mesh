package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.assertIsEnabled
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.onNodeWithContentDescription
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performTextInput
import androidx.compose.ui.test.performScrollToIndex
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.hosts.SecretBox
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AgentScreen
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.messages.PREVIEW_LINES
import dev.agentmesh.android.ui.messages.isLong
import dev.agentmesh.android.ui.messages.preview
import dev.agentmesh.gen.agentmesh.v1.AgentPhase
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.MessageType
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

private object Plain : SecretBox {
    override fun seal(plain: String) = plain
    override fun open(sealed: String) = sealed
}

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi")
class MessagesTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()

    private val theme = Themes.resolve(Themes.parse(File("src/main/assets/themes/dracula.json").readText()))
    private val api = RecordingApi()

    private val lead = agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead")
    private val worker = agent("w1", parent = "lead", name = "shop-fix", state = AgentState.AGENT_STATE_IDLE, at = 2_000)
        .toBuilder().setPhase(AgentPhase.AGENT_PHASE_PLANNING).build()

    private fun runtime(): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        return HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined)).also {
            it.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
            // Make the runtime's api() available without a connection: build it through the connection's factory.
            it.connection.start()
        }
    }

    private fun show(rt: HostRuntime, agentId: String = "w1", tab: dev.agentmesh.android.notify.Tab = dev.agentmesh.android.notify.Tab.Messages) {
        val container = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> api })
        compose.setContent {
            CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AgentScreen(rt, agentId, {}, tab) } }
        }
    }

    private val plan = message("m1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "## The plan\n\nChange the **typo**.", at = 10_000)

    @Test fun `a plan that waits on the user is approved in place, and a planning worker gets ApproveAgent`() {
        val rt = runtime()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        rt.onFrame(messageSent(1, plan, at = 10_000))
        show(rt)
        compose.onNodeWithText("waiting for you").assertIsDisplayed()
        compose.onNodeWithText("Approve").assertIsEnabled()
        // Typed changes are not an approval.
        compose.onNodeWithTag("answer").performTextInput("rename it")
        compose.onNodeWithText("Approve").assertIsNotEnabled()
        compose.onNodeWithText("Request changes").performClick()
        compose.waitUntil(5_000) { api.sent.isNotEmpty() }
        assertEquals(Triple("w1", MessageType.MESSAGE_TYPE_ANSWER, "changes: rename it"), api.sent.single())
    }

    @Test fun `approve calls ApproveAgent for a planning worker and a plain answer otherwise`() = runBlocking {
        val rt = runtime()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker, agent("w2", parent = "lead", name = "other"))))
        rt.approve("w1")
        rt.approve("w2")
        assertEquals(listOf("w1"), api.approved)
        assertEquals(listOf(Triple("w2", MessageType.MESSAGE_TYPE_ANSWER, "approved")), api.sent)
        // The stored answer joined the state at once, so the plan stops waiting before the event arrives.
        assertEquals(setOf("approved-1", "sent-1"), rt.state.value.messages.keys)
    }

    @Test fun `a question is answered with the text typed`() {
        val rt = runtime()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        rt.onFrame(messageSent(1, message("q1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_QUESTION, body = "Which branch?", at = 10_000)))
        show(rt)
        compose.onNodeWithText("Send answer").assertIsNotEnabled()
        compose.onNodeWithTag("answer").performTextInput("main")
        compose.onNodeWithText("Send answer").performClick()
        compose.waitUntil(5_000) { api.sent.isNotEmpty() }
        assertEquals(Triple("w1", MessageType.MESSAGE_TYPE_ANSWER, "main"), api.sent.single())
    }

    @Test fun `once the lead answered, nothing waits and no form shows`() {
        val rt = runtime()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        rt.onFrame(messageSent(1, plan, at = 10_000))
        rt.onFrame(messageSent(2, message("a1", from = "lead", to = "w1", type = MessageType.MESSAGE_TYPE_ANSWER, body = "approved", at = 11_000), at = 11_000))
        show(rt)
        compose.onNodeWithText("The plan").assertIsDisplayed()
        compose.onAllNodesWithTag("answer").assertCountEquals(0)
    }

    @Test fun `a failed send shows the host's reason and keeps the text`() {
        api.failSend = IllegalStateException("The host refused: agent cap")
        val rt = runtime()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        show(rt)
        compose.onNodeWithText("Message the agent").performTextInput("do it")
        compose.onNodeWithContentDescriptionCompat("Send").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTextCompat("The host refused: agent cap") }
        compose.onNodeWithText("do it").assertIsDisplayed()
    }

    @Test fun `a long report is cut to a preview until Show all`() {
        val long = (1..60).joinToString("\n") { "line $it of the report" }
        assertTrue(isLong(long)); assertEquals(false, isLong("short"))
        assertEquals(PREVIEW_LINES + 1, preview(long).lines().size)
        val rt = runtime()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        rt.onFrame(messageSent(1, message("d1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_DONE, body = long, at = 10_000)))
        show(rt)
        compose.onNodeWithText("Show all").assertIsDisplayed()
    }

    /**
     * The transcript's size must not matter: 2,000 messages compose only the rows on screen (plus a screenful of
     * lookahead), start at the newest, and a message that arrives while the user is at the bottom keeps the list
     * there. (The frame-time half of the bar needs a phone: android/README.md, "Performance".)
     */
    @Test fun `two thousand messages compose a bounded number of rows and follow new ones at the bottom`() {
        val rt = runtime()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        val many = (1..2000).map { message("big-%04d".format(it), from = if (it % 2 == 0) "w1" else "lead", to = if (it % 2 == 0) "lead" else "w1", type = MessageType.MESSAGE_TYPE_INFO, body = "message number $it", at = 1_000L + it) }
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        runBlocking { many.forEach { rt.onFrame(messageSent(it.createdAt.seconds * 1000L + it.createdAt.nanos / 1_000_000L, it)) } }
        show(rt)
        compose.waitForIdle()
        compose.onNodeWithText("message number 2000").assertIsDisplayed()
        val rows = compose.onAllNodesWithTag("message").fetchSemanticsNodes().size
        assertTrue("composed $rows rows of 2000", rows in 1..40)
        // A new message at the bottom stays in view.
        rt.onFrame(messageSent(5_000, message("big-2001", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO, body = "message number 2001", at = 9_000)))
        compose.waitForIdle()
        compose.onNodeWithText("message number 2001").assertIsDisplayed()
        // Scrolled up, the user isn't dragged back down by the next one.
        compose.onNodeWithTag("messages").performScrollToIndex(100)
        compose.waitForIdle()
        rt.onFrame(messageSent(5_001, message("big-2002", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO, body = "message number 2002", at = 9_001)))
        compose.waitForIdle()
        assertEquals(0, compose.onAllNodesWithText("message number 2002").fetchSemanticsNodes().size)
    }
}

private fun androidx.compose.ui.test.junit4.ComposeContentTestRule.onNodeWithContentDescriptionCompat(d: String) =
    onNodeWithContentDescription(d)
private fun androidx.compose.ui.test.junit4.ComposeContentTestRule.onAllNodesWithTextCompat(t: String) = onAllNodesWithText(t).fetchSemanticsNodes().isNotEmpty()
