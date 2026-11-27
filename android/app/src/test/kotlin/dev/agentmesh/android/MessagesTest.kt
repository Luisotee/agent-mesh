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

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], qualifiers = "w411dp-h900dp-xxhdpi")
class MessagesTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()

    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val api = RecordingApi()

    private val lead = agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead")
    private val worker = agent("w1", parent = "lead", name = "shop-fix", state = AgentState.AGENT_STATE_IDLE, at = 2_000)
        .toBuilder().setPhase(AgentPhase.AGENT_PHASE_PLANNING).build()

    private fun runtime(retryMs: Long = 1_000): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        return HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined), messageRetryMs = retryMs).also {
            it.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
            // Make the runtime's api() available without a connection: build it through the connection's factory.
            it.connection.start()
        }
    }

    /** A snapshot, and the run's stored messages loaded (the empty history of the fake host). */
    private fun sync(rt: HostRuntime, agents: List<dev.agentmesh.gen.agentmesh.v1.Agent>) {
        rt.onFrame(snapshot(listOf(run("r1")), agents))
        rt.loadMessages()
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
        sync(rt, listOf(lead, worker))
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

    @Test fun `approve calls ApproveAgent for any plan, planning worker or not, so the host's own guard decides`() = runBlocking {
        val rt = runtime()
        sync(rt, listOf(lead, worker, agent("w2", parent = "lead", name = "other")))
        rt.onFrame(messageSent(1, plan, at = 10_000))
        rt.onFrame(messageSent(2, message("m2", from = "w2", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "plan", at = 10_001), at = 10_001))
        rt.approve("w1")
        rt.approve("w2")
        assertEquals(listOf("w1", "w2"), api.approved)
        assertEquals(emptyList<Triple<String, MessageType, String>>(), api.sent)
        // The stored answer joined the state at once, so the plan stops waiting before the event arrives.
        assertEquals(setOf("m1", "m2", "approved-1", "approved-2"), rt.state.value.messages.keys)
    }

    @Test fun `an answer is bound to the plan the user read, not to a newer one the author sent since`() = runBlocking {
        val rt = runtime()
        sync(rt, listOf(lead, worker))
        rt.onFrame(messageSent(1, plan, at = 10_000))
        // The user opened the plan m1; the worker sent a revised plan before they tapped.
        rt.onFrame(messageSent(2, message("m2", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "revised plan", at = 12_000), at = 12_000))
        val e = runCatching { rt.approve("w1", "m1") }.exceptionOrNull()
        assertTrue("got $e", e is AlreadyAnswered && "newer" in e.message!!)
        assertEquals(emptyList<String>(), api.approved)
        val e2 = runCatching { rt.requestChanges("w1", "rename", "m1") }.exceptionOrNull()
        assertTrue(e2 is AlreadyAnswered)
        assertEquals(emptyList<Triple<String, MessageType, String>>(), api.sent)
        // The plan that does wait can be approved.
        rt.approve("w1", "m2")
        assertEquals(listOf("w1"), api.approved)
    }

    @Test fun `a row of the host's own list is answered before the history has loaded`() = runBlocking {
        api.waitingList = listOf(waiting(dev.agentmesh.gen.agentmesh.v1.WaitingKind.WAITING_KIND_PLAN, "w1", message = plan))
        val rt = runtime()
        // The snapshot is in; the run's stored messages are not (no loadMessages): the host's list still says what waits.
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        assertTrue(rt.state.value.waiting != null)
        assertTrue("r1" !in rt.state.value.messagesLoaded)
        rt.approve("w1", "m1")
        assertEquals(listOf("w1"), api.approved)
        // A row the host no longer lists is stale.
        api.waitingList = emptyList()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        val e = runCatching { rt.approve("w1", "m1") }.exceptionOrNull()
        assertTrue("got $e", e is AlreadyAnswered)
    }

    @Test fun `a stale Approve sends nothing once the plan was answered elsewhere`() = runBlocking {
        val rt = runtime()
        sync(rt, listOf(lead, worker))
        rt.onFrame(messageSent(1, plan, at = 10_000))
        rt.onFrame(messageSent(2, message("a1", from = "lead", to = "w1", type = MessageType.MESSAGE_TYPE_ANSWER, body = "approved", at = 11_000), at = 11_000))
        val e = runCatching { rt.approve("w1") }.exceptionOrNull()
        assertTrue("got $e", e is AlreadyAnswered)
        assertEquals(emptyList<String>(), api.approved)
        // Nothing to approve (no plan or question from that agent) is refused too, not sent as a bare "approved".
        val none = runCatching { rt.approve("lead") }.exceptionOrNull()
        assertTrue("got $none", none is NothingToAnswer)
        assertTrue(none!!.message!!.endsWith("nothing was sent."))
        assertTrue(runCatching { rt.answer("w1", "x") }.exceptionOrNull() is AlreadyAnswered)
        assertTrue(runCatching { rt.requestChanges("w1", "x") }.exceptionOrNull() is AlreadyAnswered)
        assertEquals(emptyList<Triple<String, MessageType, String>>(), api.sent)
    }

    @Test fun `no action is taken on a conversation that isn't loaded, and a failed load says so`() = runBlocking {
        val rt = runtime()
        // The snapshot is in but the run's messages are not: an empty history is not "nothing to answer".
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        rt.onFrame(messageSent(1, plan, at = 10_000))
        assertTrue(runCatching { rt.approve("w1") }.exceptionOrNull() is NotLoaded)
        assertEquals(emptyList<String>(), api.approved)
        // Loaded: it goes through.
        rt.loadMessages()
        rt.approve("w1")
        assertEquals(listOf("w1"), api.approved)
    }

    @Test fun `a question is answered with the text typed`() {
        val rt = runtime()
        sync(rt, listOf(lead, worker))
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
        sync(rt, listOf(lead, worker))
        rt.onFrame(messageSent(1, plan, at = 10_000))
        rt.onFrame(messageSent(2, message("a1", from = "lead", to = "w1", type = MessageType.MESSAGE_TYPE_ANSWER, body = "approved", at = 11_000), at = 11_000))
        show(rt)
        compose.onNodeWithText("The plan").assertIsDisplayed()
        compose.onAllNodesWithTag("answer").assertCountEquals(0)
    }

    @Test fun `a failed send shows the host's reason and keeps the text`() {
        api.failSend = IllegalStateException("The host refused: agent cap")
        val rt = runtime()
        sync(rt, listOf(lead, worker))
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
        sync(rt, listOf(lead, worker))
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
        sync(rt, listOf(lead, worker))
        val many = (1..2000).map { message("big-%04d".format(it), from = if (it % 2 == 0) "w1" else "lead", to = if (it % 2 == 0) "lead" else "w1", type = MessageType.MESSAGE_TYPE_INFO, body = "message number $it", at = 1_000L + it) }
        sync(rt, listOf(lead, worker))
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

    /** The list that loads after the tab first composed (the usual case) must still let the user read old messages. */
    @Test fun `messages that load after the tab opened don't drag a reader scrolled up to the bottom`() {
        val rt = runtime()
        sync(rt, listOf(lead, worker))
        show(rt)
        compose.waitForIdle()
        compose.onNodeWithText("No messages yet.").assertIsDisplayed()
        val many = (1..300).map { message("late-%04d".format(it), from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO, body = "message number $it", at = 1_000L + it) }
        many.forEachIndexed { i, m -> rt.onFrame(messageSent(1L + i, m)) }
        compose.waitForIdle()
        compose.onNodeWithText("message number 300").assertIsDisplayed()
        compose.onNodeWithTag("messages").performScrollToIndex(100)
        compose.waitForIdle()
        rt.onFrame(messageSent(900, message("late-0301", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO, body = "message number 301", at = 5_000)))
        compose.waitForIdle()
        assertEquals("the reader stayed where they were", 0, compose.onAllNodesWithText("message number 301").fetchSemanticsNodes().size)
        assertEquals(0, compose.onAllNodesWithText("message number 300").fetchSemanticsNodes().size)
    }

    @Test fun `an empty tab shows a skeleton until the run's history is in, and offers a retry when the load gave up`() {
        val rt = runtime(retryMs = 5)
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        show(rt)
        compose.waitForIdle()
        compose.onNodeWithContentDescription("Loading the messages").assertIsDisplayed()
        // The loads gave up.
        api.failListings = 100
        rt.loadMessages()
        compose.waitUntil(10_000) { rt.state.value.messagesFailed.isNotEmpty() }
        compose.waitForIdle()
        compose.onNodeWithText("Couldn't load the messages.").assertIsDisplayed()
        api.failListings = 0
        compose.onNodeWithTag("reload-messages").performClick()
        compose.waitUntil(5_000) { rt.state.value.messagesLoaded.isNotEmpty() }
        compose.waitForIdle()
        compose.onNodeWithText("No messages yet.").assertIsDisplayed()
    }

    /** A run a lead starts while the phone is connected: its plan can be approved at once, and its tab isn't left Loading. */
    @Test fun `a run started after connecting can be approved and answered, and its tab isn't stuck Loading`() = runBlocking {
        val rt = runtime()
        sync(rt, listOf(lead, worker))
        val lead2 = agent("lead2", runId = "r2", role = AgentRole.AGENT_ROLE_ROOT, name = "api-lead")
        val w2 = agent("w2", runId = "r2", parent = "lead2", name = "api-fix").toBuilder().setPhase(AgentPhase.AGENT_PHASE_PLANNING).build()
        rt.onFrame(runStarted(1, run("r2")))
        rt.onFrame(agentSpawned(2, lead2)); rt.onFrame(agentSpawned(3, w2))
        rt.onFrame(messageSent(4, message("p2", runId = "r2", from = "w2", to = "lead2", type = MessageType.MESSAGE_TYPE_PLAN, body = "the plan", at = 20_000), at = 20_000))
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance())) // (the fixture connection never answers)
        rt.awaitFresh("w2") // returns: nothing to wait for
        rt.approve("w2")
        assertEquals(listOf("w2"), api.approved)
        rt.onFrame(messageSent(5, message("q2", runId = "r2", from = "w2", to = "lead2", type = MessageType.MESSAGE_TYPE_QUESTION, body = "which?", at = 99_000), at = 99_000))
        rt.answer("w2", "this one")
        assertEquals(Triple("w2", MessageType.MESSAGE_TYPE_ANSWER, "this one"), api.sent.single())
    }

    @Test fun `Try again reloads only the runs that failed`() {
        val rt = runtime(retryMs = 5)
        api.failRuns = setOf("r2")
        rt.onFrame(snapshot(listOf(run("r1"), run("r2")), listOf(lead, worker)))
        rt.loadMessages()
        compose.waitUntil(10_000) { rt.state.value.messagesFailed == setOf("r2") && rt.state.value.messagesLoaded == setOf("r1") }
        val r1Calls = api.listCalls.count { it.first == "r1" }
        api.failRuns = emptySet()
        rt.reloadMessages()
        compose.waitUntil(5_000) { rt.state.value.messagesLoaded == setOf("r1", "r2") }
        assertEquals("the healthy run wasn't loaded again", r1Calls, api.listCalls.count { it.first == "r1" })
    }

}

private fun androidx.compose.ui.test.junit4.ComposeContentTestRule.onNodeWithContentDescriptionCompat(d: String) =
    onNodeWithContentDescription(d)
private fun androidx.compose.ui.test.junit4.ComposeContentTestRule.onAllNodesWithTextCompat(t: String) = onAllNodesWithText(t).fetchSemanticsNodes().isNotEmpty()
