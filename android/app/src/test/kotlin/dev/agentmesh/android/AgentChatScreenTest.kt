package dev.agentmesh.android

import android.app.Application
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.assertIsEnabled
import androidx.compose.ui.test.assertIsNotDisplayed
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performScrollTo
import androidx.compose.ui.test.performScrollToIndex
import androidx.compose.ui.test.performTextInput
import androidx.test.core.app.ApplicationProvider
import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AgentScreen
import dev.agentmesh.android.ui.AllHostsDockScreen
import dev.agentmesh.android.ui.HomeScreen
import dev.agentmesh.android.ui.rememberSidebar
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.UsageScreen
import dev.agentmesh.android.ui.settings.HostSettingsScreen
import dev.agentmesh.gen.agentmesh.v1.Actor
import dev.agentmesh.gen.agentmesh.v1.ActorClass
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.ClaudeUsage
import dev.agentmesh.gen.agentmesh.v1.ClaudeUsageWindow
import dev.agentmesh.gen.agentmesh.v1.CliCost
import dev.agentmesh.gen.agentmesh.v1.GetCostResponse
import dev.agentmesh.gen.agentmesh.v1.GetUsageResponse
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse
import dev.agentmesh.gen.agentmesh.v1.Reopen
import dev.agentmesh.gen.agentmesh.v1.Setting
import dev.agentmesh.gen.agentmesh.v1.SettingSchema
import dev.agentmesh.gen.agentmesh.v1.TranscriptEntry
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind
import dev.agentmesh.gen.agentmesh.v1.WaitingKind
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File
import androidx.compose.ui.test.assertTextContains
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.performTouchInput
import androidx.compose.ui.test.longClick

/** The chat-first agent screen: its tree strip, the details sheet, Stop and Retire, the terminal toggle, quoting a reply. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w411dp-h1600dp-xxhdpi")
class AgentChatScreenTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val app get() = ApplicationProvider.getApplicationContext<Application>()
    private val container get() = (app as MeshApp).container
    private lateinit var api: RecordingApi

    private fun entry(id: String, kind: TranscriptKind, text: String = ""): TranscriptEntry = TranscriptEntry.newBuilder().setId(id).setKind(kind).setText(text).build()
    private fun page(entries: List<TranscriptEntry>) =
        ReadTranscriptResponse.newBuilder().setSupported(true).setSessionId("s1").addAllEntries(entries).setStartOffset(0).setEndOffset(100).setAtStart(true).build()

    private val lead = agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead", state = AgentState.AGENT_STATE_IDLE)
    private val worker = agent("w1", parent = "lead", name = "shop-fix", state = AgentState.AGENT_STATE_WORKING).toBuilder().setTerminalId("t1").setBranch("mesh/r1/fix").setWorktree("/wt/fix").setSessionId("s1").setModel("sonnet").build()
    private val reviewer = agent("rev", parent = "w1", role = AgentRole.AGENT_ROLE_REVIEWER, name = "reviewer-1")

    private fun host(vararg agents: dev.agentmesh.gen.agentmesh.v1.Agent, actor: ActorClass = ActorClass.ACTOR_CLASS_DEVICE): HostRuntime {
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

    private fun show(rt: HostRuntime, agentId: String = "w1", onAgent: (String) -> Unit = {}) =
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { AgentScreen(rt, agentId, {}, onAgent = onAgent) } } }

    private fun fresh(transcript: List<TranscriptEntry> = emptyList()) {
        api = RecordingApi().apply { this.transcript = { _, _, _ -> page(transcript) } }
        NotificationsTest.api = api
    }

    @Test fun `the strip holds the tree, marks the open agent, folds reviewers into a chip, and a tap switches agent`() {
        fresh()
        val rt = host(lead, worker, reviewer)
        var opened = ""
        show(rt, onAgent = { opened = it })
        compose.onNodeWithTag("tree-lead").assertIsDisplayed()
        compose.onNodeWithTag("tree-w1").assertIsDisplayed()
        // The reviewer is a count chip, not a chip of its own.
        compose.onAllNodesWithTag("tree-rev").assertCountEquals(0)
        compose.onNodeWithText("·1 review").assertIsDisplayed()
        compose.onNodeWithTag("tree-lead").performClick()
        assertEquals("lead", opened)
        // The agent that is open is not a button.
        opened = ""
        compose.onNodeWithTag("tree-w1").performClick()
        assertEquals("", opened)
    }

    @Test fun `a lone agent has no strip`() {
        fresh()
        val rt = host(lead)
        show(rt, "lead")
        compose.onAllNodesWithTag("tree-strip").assertCountEquals(0)
    }

    @Test fun `a reviewer that is open is on the strip, selected`() {
        fresh()
        val rt = host(lead, worker, reviewer)
        show(rt, "rev")
        compose.onNodeWithTag("tree-rev").assertIsDisplayed()
    }

    @Test fun `the details hold the agents, the facts, a message and Stop, and the cost is read only when opened`() {
        fresh()
        api.cost = GetCostResponse.newBuilder().setScope("run r1").setTotalTokens(1_200_000).setUsd(4.5).build()
        val rt = host(lead, worker, reviewer)
        show(rt)
        compose.onNodeWithTag("details-open").performClick()
        compose.onNodeWithTag("details-agents").assertIsDisplayed()
        compose.onNodeWithTag("details-agent-rev").assertIsDisplayed()
        compose.onNodeWithText("mesh/r1/fix").assertIsDisplayed()
        compose.onNodeWithText("/wt/fix").assertIsDisplayed()
        compose.onNodeWithTag("details-send").assertIsEnabled()
        assertEquals("the cost is not read until it is opened", emptyList<Pair<String, String>>(), api.costCalls.toList())
        compose.onNodeWithTag("details-cost").performClick()
        compose.waitUntil(5_000) { api.costCalls.isNotEmpty() }
        assertEquals(listOf("r1" to ""), api.costCalls.toList())
        compose.onNodeWithText("1.2M tokens · ≈ $4.50 at API prices").assertIsDisplayed()
    }

    @Test fun `a read-only device sees the details and none of the actions`() {
        fresh()
        val rt = host(lead, worker, actor = ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE)
        show(rt)
        compose.onNodeWithTag("details-open").performClick()
        compose.onNodeWithText("mesh/r1/fix").assertIsDisplayed()
        compose.onAllNodesWithTag("details-send").assertCountEquals(0)
        compose.onAllNodesWithTag("details-stop").assertCountEquals(0)
        compose.onAllNodesWithTag("details-retire").assertCountEquals(0)
    }

    @Test fun `Stop asks first, sends StopAgent on yes, and sends nothing on cancel`() {
        fresh()
        val rt = host(lead, worker)
        show(rt)
        compose.onNodeWithTag("details-open").performClick()
        compose.onNodeWithTag("details-stop").performClick()
        compose.onNodeWithText("Stop shop-fix?").assertIsDisplayed()
        compose.onNodeWithTag("action-cancel").performClick()
        assertEquals(emptyList<String>(), api.lifecycle.toList())
        compose.onNodeWithTag("details-stop").performClick()
        compose.onNodeWithTag("action-confirm").performClick()
        compose.waitUntil(5_000) { api.lifecycle.isNotEmpty() }
        assertEquals(listOf("stop w1"), api.lifecycle.toList())
    }

    @Test fun `Retire asks first and the host's refusal is shown in its own words`() {
        fresh()
        api.failLifecycle = ApiException(Code.FAILED_PRECONDITION, "[failed_precondition] w1 has an unpushed branch")
        val rt = host(lead, worker)
        show(rt)
        compose.onNodeWithTag("details-open").performClick()
        compose.onNodeWithTag("details-retire").performClick()
        compose.onNodeWithText("Retire shop-fix?").assertIsDisplayed()
        compose.onNodeWithTag("action-confirm").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("action-error").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("w1 has an unpushed branch").assertIsDisplayed()
        assertEquals(emptyList<String>(), api.lifecycle.toList())
    }

    @Test fun `a stopped agent can't be stopped or retired again`() {
        fresh()
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_RETIRED).build())
        show(rt)
        compose.onNodeWithTag("details-open").performClick()
        compose.onNodeWithTag("details-stop").assertIsNotEnabled()
        compose.onNodeWithTag("details-retire").assertIsNotEnabled()
    }

    @Test fun `the terminal toggle swaps the chat for the terminal and back`() {
        fresh()
        val rt = host(lead, worker)
        show(rt)
        compose.onNodeWithTag("chat").assertIsDisplayed()
        compose.onNodeWithTag("terminal-toggle").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("chat").fetchSemanticsNodes().isEmpty() }
        compose.onNodeWithTag("terminal-toggle").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("chat").fetchSemanticsNodes().isNotEmpty() }
    }

    @Test fun `a long press on a reply offers Quote, which opens the reply with the lines marked`() {
        fresh(listOf(entry("1", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "On it.\nSecond line.")))
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("chat-assistant").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("chat-assistant").performTouchInput { longClick() }
        compose.onNodeWithTag("msg-copy").assertIsDisplayed()
        compose.onNodeWithTag("msg-quote").performClick()
        compose.onNodeWithTag("prompt").assertTextContains("> On it.\n> Second line.", substring = true)
    }

    @Test fun `a stopped agent has no composer to quote into, so the menu does not offer Quote`() {
        fresh(listOf(entry("1", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "On it.")))
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_EXITED).build())
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("chat-assistant").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("chat-assistant").performTouchInput { longClick() }
        compose.onNodeWithTag("msg-copy").assertIsDisplayed()
        compose.onAllNodesWithTag("msg-quote").assertCountEquals(0)
    }

    @Test fun `while the agent works an empty box offers Stop, typing turns it into Send, and Stop interrupts`() {
        fresh()
        val rt = host(lead, worker)
        show(rt)
        compose.onNodeWithTag("stop").assertIsDisplayed()
        compose.onAllNodesWithTag("send").assertCountEquals(0)
        compose.onNodeWithTag("stop").performClick()
        compose.waitUntil(5_000) { api.interrupts.isNotEmpty() }
        assertEquals(listOf("w1"), api.interrupts.toList())
        compose.onNodeWithTag("prompt").performTextInput("and the readme")
        compose.onNodeWithTag("send").assertIsDisplayed()
        compose.onAllNodesWithTag("stop").assertCountEquals(0)
    }

    @Test fun `an agent waiting at a prompt has no Stop, as Esc there would deny the dialog`() {
        fresh()
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_NEEDS_INPUT).build())
        show(rt)
        compose.onAllNodesWithTag("stop").assertCountEquals(0)
        compose.onNodeWithTag("prompt-at-prompt").assertIsDisplayed()
    }

    @Test fun `an agent that is not Claude has no Stop`() {
        fresh()
        val rt = host(lead, worker.toBuilder().setKind("opencode").build())
        show(rt)
        compose.onAllNodesWithTag("stop").assertCountEquals(0)
        compose.onNodeWithTag("send").assertIsNotEnabled()
    }

    @Test fun `a prompt the host queues shows under the conversation, can be taken back to edit or dropped`() {
        fresh()
        api.queueNext = "working"
        val rt = host(lead, worker)
        show(rt)
        compose.onNodeWithTag("prompt").performTextInput("also fix the readme")
        compose.onNodeWithTag("send").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-q1").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("Sent when it finishes its turn").assertIsDisplayed()
        // Sent: the box is empty.
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("stop").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("queued-edit-q1").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-q1").fetchSemanticsNodes().isEmpty() }
        assertEquals(listOf("q1"), api.withdrawn.toList())
        compose.onNodeWithTag("prompt").assertTextContains("also fix the readme", substring = true)
    }

    @Test fun `a queued prompt the host listed is dropped with the cross`() {
        fresh()
        api.queued["w1"] = listOf(queuedPrompt("p1", "w1", "later", held = "draft"))
        val rt = host(lead, worker)
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-p1").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("Held: its input box holds a draft. Clear it in the terminal, or send this anyway").assertIsDisplayed()
        compose.onNodeWithTag("queued-drop-p1").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-p1").fetchSemanticsNodes().isEmpty() }
        assertEquals(listOf("p1"), api.withdrawn.toList())
        // Dropped, not edited: nothing is put in the box.
        compose.onNodeWithTag("prompt").assertTextContains("", substring = true)
    }

    @Test fun `a prompt held by a draft is sent over it after the draft is named and confirmed`() {
        fresh()
        api.queued["w1"] = listOf(queuedPrompt("p1", "w1", "later", held = "draft", heldText = "half a thou"))
        val rt = host(lead, worker)
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-anyway-p1").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("queued-anyway-p1").performClick()
        compose.onNodeWithTag("send-anyway-draft").assertTextContains("half a thou", substring = true)
        compose.onNodeWithTag("send-anyway-confirm").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-p1").fetchSemanticsNodes().isEmpty() }
        assertEquals(listOf(listOf("w1", "p1", "half a thou")), api.overDrafts.toList())
    }

    @Test fun `a prompt held for another reason has no Send anyway`() {
        fresh()
        api.queued["w1"] = listOf(queuedPrompt("p1", "w1", "later", held = "working"), queuedPrompt("p2", "w1", "then", held = "draft"))
        val rt = host(lead, worker)
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-p1").fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodesWithTag("queued-anyway-p1").assertCountEquals(0)
        compose.onAllNodesWithTag("queued-anyway-p2").assertCountEquals(0) // no draft text to name
    }

    @Test fun `a queue that arrives under a long conversation is brought into view`() {
        fresh((1..40).map { entry("$it", if (it % 2 == 1) TranscriptKind.TRANSCRIPT_KIND_USER else TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "Line $it of a long conversation that fills the screen.\n\nWith a second paragraph.") })
        api.queued["w1"] = listOf(queuedPrompt("p1", "w1", "later", held = "working"))
        val rt = host(lead, worker)
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-p1").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("queued-p1").assertIsDisplayed()
    }

    @Test fun `a stopped agent's queue can be dropped but not taken back into a box that is off`() {
        fresh()
        api.queued["w1"] = listOf(queuedPrompt("p1", "w1", "later", held = "stopped"))
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_EXITED).build())
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-p1").fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodesWithTag("queued-edit-p1").assertCountEquals(0)
        compose.onNodeWithTag("queued-drop-p1").assertIsDisplayed()
    }

    @Test fun `Send empties the box and the prompt is a bubble at once, before the host has answered`() {
        fresh()
        val gate = kotlinx.coroutines.CompletableDeferred<Unit>()
        api.promptGate = gate
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        compose.onNodeWithTag("prompt").performTextInput("do it now")
        compose.onNodeWithTag("send").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("outgoing-sending").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("Sending\u2026").assertIsDisplayed()
        compose.onNodeWithTag("outgoing-text").assertTextContains("do it now")
        compose.onNodeWithTag("prompt").assertTextContains("", substring = false)
        // The host queues it: the queue's bubble takes its place.
        api.queueNext = "working"
        gate.complete(Unit)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-q1").fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodesWithTag("outgoing-sending").assertCountEquals(0)
        assertEquals(listOf("w1" to "do it now"), api.prompts.toList())
    }

    @Test fun `a prompt the host typed stays as Sent until the record has it, and is then the record's own bubble`() {
        fresh()
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        compose.waitUntil(5_000) { rt.outbox.value.isEmpty() && compose.onAllNodesWithTag("prompt").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("prompt").performTextInput("typed at once")
        compose.onNodeWithTag("send").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("outgoing-sent").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("Sent").assertIsDisplayed()
        // The record shows it (the next read): the transcript's bubble replaces this one.
        api.transcript = { _, _, _ -> page(listOf(entry("1", TranscriptKind.TRANSCRIPT_KIND_USER, "typed at once"))) }
        compose.waitUntil(8_000) { compose.onAllNodesWithTag("outgoing-sent").fetchSemanticsNodes().isEmpty() }
        compose.onAllNodesWithTag("chat-user").assertCountEquals(1)
        assertTrue(rt.outbox.value.isEmpty())
    }

    @Test fun `a refused send stays on its bubble with Retry and Edit, and the refusal is the host's words`() {
        fresh()
        api.failPrompt = RuntimeException("its input box holds a draft")
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        compose.onNodeWithTag("prompt").performTextInput("try this")
        compose.onNodeWithTag("send").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("outgoing-failed").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText("Not sent: its input box holds a draft").assertIsDisplayed()
        // Retry sends it again, and the bubble goes when the host has it.
        api.failPrompt = null
        compose.onNodeWithTag("outgoing-retry").performClick()
        compose.waitUntil(5_000) { api.prompts.isNotEmpty() }
        assertEquals(listOf("w1" to "try this"), api.prompts.toList())
    }

    @Test fun `Edit on a refused bubble takes the text back into the box, after what was typed since, and Drop forgets it`() {
        fresh()
        api.failPrompt = RuntimeException("no")
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        compose.onNodeWithTag("prompt").performTextInput("step 1")
        compose.onNodeWithTag("send").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("outgoing-failed").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("prompt").performTextInput("step 2")
        compose.onNodeWithTag("outgoing-edit").performClick()
        compose.onAllNodesWithTag("outgoing-failed").assertCountEquals(0)
        compose.onNodeWithTag("prompt").assertTextContains("step 1", substring = true)
        compose.onNodeWithTag("prompt").assertTextContains("step 2", substring = true)
        compose.onNodeWithTag("send").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("outgoing-failed").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("outgoing-drop").performClick()
        compose.onAllNodesWithTag("outgoing-failed").assertCountEquals(0)
        assertTrue(rt.outbox.value.isEmpty())
    }

    @Test fun `a refused bubble is still there after the view was left for the terminal and came back`() {
        fresh()
        api.failPrompt = RuntimeException("its input box holds a draft")
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        compose.onNodeWithTag("prompt").performTextInput("do not lose me")
        compose.onNodeWithTag("send").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("outgoing-failed").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("terminal-toggle").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("chat").fetchSemanticsNodes().isEmpty() }
        compose.onNodeWithTag("terminal-toggle").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("outgoing-failed").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("outgoing-text").assertTextContains("do not lose me")
    }

    @Test fun `several prompts sent in a row reach the host in the order they were sent`() {
        fresh()
        val gate = kotlinx.coroutines.CompletableDeferred<Unit>()
        api.promptGate = gate
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        for (t in listOf("one", "two", "three")) {
            compose.onNodeWithTag("prompt").performTextInput(t)
            compose.onNodeWithTag("send").performClick()
        }
        compose.waitUntil(5_000) { rt.outbox.value.size == 3 }
        gate.complete(Unit)
        compose.waitUntil(5_000) { api.prompts.size == 3 }
        assertEquals(listOf("one", "two", "three"), api.prompts.map { it.second })
    }

    @Test fun `a read-only device sees the queue but not the buttons to change it`() {
        fresh()
        api.queued["w1"] = listOf(queuedPrompt("p1", "w1", "later"))
        val rt = host(lead, worker, actor = ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE)
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("queued-p1").fetchSemanticsNodes().isNotEmpty() }
        compose.onAllNodesWithTag("queued-edit-p1").assertCountEquals(0)
        compose.onAllNodesWithTag("queued-drop-p1").assertCountEquals(0)
    }

    @Test fun `what is typed is kept on the device as the agent's draft, and forgotten once sent`() {
        fresh()
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        compose.onNodeWithTag("prompt").performTextInput("half a thought")
        compose.waitUntil(5_000) { container.prefs.draft(rt.hostId, "w1") == "half a thought" }
        compose.onNodeWithTag("send").performClick()
        compose.waitUntil(5_000) { api.prompts.isNotEmpty() }
        compose.waitUntil(5_000) { container.prefs.draft(rt.hostId, "w1").isEmpty() }
    }

    @Test fun `a draft kept on the device opens in the box`() {
        fresh()
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        container.prefs.setDraft(rt.hostId, "w1", "from last time")
        show(rt)
        compose.onNodeWithTag("prompt").assertTextContains("from last time", substring = true)
    }

    @Test fun `Enter on a list item continues the list, and the format row wraps what is typed`() {
        fresh()
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        // The row of buttons is there only while the box is focused.
        compose.onAllNodesWithTag("format-row").assertCountEquals(0)
        compose.onNodeWithTag("prompt").performClick()
        compose.onNodeWithTag("format-row").assertIsDisplayed()
        compose.onNodeWithTag("prompt").performTextInput("- one")
        compose.onNodeWithTag("prompt").performTextInput("\n")
        compose.onNodeWithTag("prompt").assertTextContains("- one\n- ", substring = true)
        // An empty item ends the list.
        compose.onNodeWithTag("prompt").performTextInput("\n")
        compose.onNodeWithTag("prompt").assertTextContains("- one\n", substring = true)
        compose.onNodeWithTag("format-bold").performClick()
        compose.onNodeWithTag("prompt").assertTextContains("****", substring = true)
    }

    @Test fun `the user's own words are a bubble and a pause starts a new time label`() {
        val at = { ms: Long -> com.google.protobuf.Timestamp.newBuilder().setSeconds(ms / 1000).build() }
        val now = System.currentTimeMillis()
        fresh(listOf(
            TranscriptEntry.newBuilder().setId("1").setKind(TranscriptKind.TRANSCRIPT_KIND_USER).setText("fix the typo").setTime(at(now - 3_600_000)).build(),
            TranscriptEntry.newBuilder().setId("2").setKind(TranscriptKind.TRANSCRIPT_KIND_ASSISTANT).setText("Done.").setTime(at(now - 3_590_000)).build(),
            TranscriptEntry.newBuilder().setId("3").setKind(TranscriptKind.TRANSCRIPT_KIND_USER).setText("thanks").setTime(at(now - 60_000)).build(),
        ))
        val rt = host(lead, worker.toBuilder().setState(AgentState.AGENT_STATE_IDLE).build())
        show(rt)
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("chat-user").fetchSemanticsNodes().size == 2 }
        // The first entry and the one after a pause are labelled; the quick reply is not.
        assertEquals(2, compose.onAllNodesWithTag("chat-time").fetchSemanticsNodes().size)
    }
}
