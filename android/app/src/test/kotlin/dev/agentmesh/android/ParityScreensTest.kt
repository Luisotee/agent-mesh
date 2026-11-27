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

/** The screens of the Android parity work, on a connected host that records what the user asks of it. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class, qualifiers = "w411dp-h1600dp-xxhdpi")
class ParityScreensTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))
    private val app get() = ApplicationProvider.getApplicationContext<Application>()
    private val container get() = (app as MeshApp).container

    private lateinit var api: RecordingApi

    private fun entry(id: String, kind: TranscriptKind, text: String = "", tool: String = "", input: String = "", useId: String = ""): TranscriptEntry =
        TranscriptEntry.newBuilder().setId(id).setKind(kind).setText(text).setTool(tool).setToolInput(input).setToolUseId(useId).build()

    private fun page(entries: List<TranscriptEntry>, atStart: Boolean = true, supported: Boolean = true) =
        ReadTranscriptResponse.newBuilder().setSupported(supported).setSessionId("s1").addAllEntries(entries).setStartOffset(0).setEndOffset(100).setAtStart(atStart).build()

    /** A connected host with [agents] in run r1, whose client is [api]. */
    private fun host(vararg agents: Agent, actor: ActorClass = ActorClass.ACTOR_CLASS_DEVICE, runs: List<dev.agentmesh.gen.agentmesh.v1.Run> = listOf(run("r1", "Fix the typo"))): HostRuntime {
        val h = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val deadline = System.currentTimeMillis() + 5_000
        var rt: HostRuntime? = null
        while (rt == null && System.currentTimeMillis() < deadline) { rt = container.runtimes.value[h.id]; Thread.sleep(20) }
        rt!!
        while (rt.api() == null && System.currentTimeMillis() < deadline) Thread.sleep(20)
        rt.onState(ConnState.Ready(HelloResponse.newBuilder().setActor(Actor.newBuilder().setClass_(actor).setId("device-1")).build()))
        rt.onFrame(snapshot(runs, agents.toList()))
        return rt
    }

    private fun show(content: @androidx.compose.runtime.Composable () -> Unit) =
        compose.setContent { CompositionLocalProvider(LocalContainer provides container) { AgentMeshTheme(theme) { content() } } }

    private fun idle() = compose.waitForIdle()

    private fun waitUntilState(rt: HostRuntime, what: (dev.agentmesh.android.store.HostState) -> Boolean) {
        val deadline = System.currentTimeMillis() + 5_000
        while (!what(rt.state.value)) { check(System.currentTimeMillis() < deadline) { "timed out" }; Thread.sleep(20) }
    }

    private fun waitForText(t: String) = compose.waitUntil(5_000) { compose.onAllNodesWithText(t).fetchSemanticsNodes().isNotEmpty() }
    private fun waitForTag(t: String) = compose.waitUntil(5_000) { compose.onAllNodesWithTag(t).fetchSemanticsNodes().isNotEmpty() }

    private val worker = agent("w1", name = "shop-fix", state = AgentState.AGENT_STATE_IDLE).toBuilder().setTerminalId("t1").setKind("claude").setModel("sonnet").setEffort("high").setTier("mid").setSessionId("s1").build()

    // ── Chat ──

    @Test fun `the chat tab shows the conversation, folds tool calls, and sends what is typed`() {
        api = RecordingApi().apply {
            transcript = { _, _, back -> if (!back) page(emptyList()) else
                page(listOf(
                    entry("1", TranscriptKind.TRANSCRIPT_KIND_USER, "fix the typo"),
                    entry("2", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "On it."),
                    entry("3", TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL, tool = "Bash", input = "git diff", useId = "t1"),
                    entry("4", TranscriptKind.TRANSCRIPT_KIND_TOOL_RESULT, "no changes", useId = "t1"),
                    entry("5", TranscriptKind.TRANSCRIPT_KIND_MESH, "📬 [mesh] task from lead"),
                ))
            }
        }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForText("On it.")
        compose.onNodeWithText("fix the typo").assertIsDisplayed()
        compose.onNodeWithText("From the mesh: task from lead").assertIsDisplayed()
        // Tool calls are folded until tapped.
        compose.onNodeWithText("Ran 1 tool: Bash").assertIsDisplayed()
        compose.onAllNodesWithText("no changes").assertCountEquals0()
        compose.onNodeWithText("Ran 1 tool: Bash").performClick()
        compose.onNodeWithText("no changes").assertIsDisplayed()
        // Typing and sending types into the agent.
        compose.onNodeWithTag("send").assertIsNotEnabled()
        compose.onNodeWithTag("prompt").performTextInput("also the readme")
        compose.onNodeWithTag("send").assertIsEnabled().performClick()
        compose.waitUntil(5_000) { api.prompts.isNotEmpty() }
        assertEquals(listOf("w1" to "also the readme"), api.prompts.toList())
        // Then the box is empty again.
        compose.waitUntil(5_000) { compose.onAllNodesWithText("also the readme").fetchSemanticsNodes().isEmpty() }
    }

    @Test fun `the host's refusal of a prompt is shown and the text stays`() {
        api = RecordingApi().apply { transcript = { _, _, _ -> page(emptyList()) }; failPrompt = ApiException(Code.FAILED_PRECONDITION, "[failed_precondition] the input box holds a draft") }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForText("Nothing said yet.")
        compose.onNodeWithTag("prompt").performTextInput("hello")
        compose.onNodeWithTag("send").performClick()
        waitForTag("prompt-error")
        compose.onNodeWithTag("prompt-error").assertIsDisplayed()
        compose.onNodeWithText("the input box holds a draft").assertIsDisplayed()
        compose.onNodeWithText("hello").assertIsDisplayed()
    }

    @Test fun `a read-only device reads but cannot type, and a stopped agent says why`() {
        api = RecordingApi().apply { transcript = { _, _, _ -> page(listOf(entry("1", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "hi"))) } }
        NotificationsTest.api = api
        val rt = host(worker, actor = ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE)
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForText("hi")
        compose.onNodeWithTag("prompt-why").assertIsDisplayed()
        compose.onNodeWithTag("send").assertIsNotEnabled()
        // And no way to change the model.
        compose.onAllNodesWithTag("change-model").assertCountEquals0()
    }

    @Test fun `a stopped agent has no terminal to type into`() {
        api = RecordingApi().apply { transcript = { _, _, _ -> page(emptyList()) } }
        NotificationsTest.api = api
        val rt = host(worker.toBuilder().setState(AgentState.AGENT_STATE_RETIRED).build())
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForTag("prompt-why")
        compose.onNodeWithText("The agent has stopped, so there is no terminal to type into. Message it from the Messages tab to bring it back.").assertIsDisplayed()
    }

    @Test fun `a CLI the host reads no record for says to use the terminal`() {
        api = RecordingApi().apply { transcript = { _, _, _ -> page(emptyList(), supported = false) } }
        NotificationsTest.api = api
        val rt = host(worker.toBuilder().setKind("agy").build())
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForTag("chat-unsupported")
        compose.onNodeWithTag("chat-unsupported").assertIsDisplayed()
    }

    @Test fun `a failed read says why and can be tried again`() {
        api = RecordingApi().apply { failRead = ApiException(Code.UNAVAILABLE, "[unavailable] the host is down") }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForTag("chat-error")
        compose.onNodeWithText("Couldn't read the conversation: the host is down").assertIsDisplayed()
        api.failRead = null
        api.transcript = { _, _, _ -> page(listOf(entry("1", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "back again"))) }
        compose.onNodeWithText("Try again").performClick()
        waitForText("back again")
    }

    @Test fun `earlier entries are offered while the record has more`() {
        api = RecordingApi().apply {
            transcript = { _, off, back ->
                when {
                    back && off != -1L -> page(listOf(entry("0", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "earlier words")), atStart = true).toBuilder().setEndOffset(50).build()
                    back -> page(listOf(entry("1", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "later words")), atStart = false).toBuilder().setStartOffset(50).build()
                    else -> page(emptyList()).toBuilder().setEndOffset(100).build()
                }
            }
        }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForText("later words")
        compose.onNodeWithTag("load-earlier").performClick()
        waitForText("earlier words")
        compose.onAllNodesWithTag("load-earlier").assertCountEquals0()
    }

    // ── Home: answering in place ──

    @Test fun `what waits on the user is the host's own list, answered in place`() {
        api = RecordingApi()
        NotificationsTest.api = api
        val plan = message("p1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "Change the typo in README", at = 10_000)
        api.waitingList = listOf(waiting(WaitingKind.WAITING_KIND_PLAN, "w1", message = plan))
        api.pages = mapOf("r1" to listOf(listOf(plan)))
        val rt = host(agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead"), agent("w1", parent = "lead", name = "shop-fix").toBuilder().setPhase(dev.agentmesh.gen.agentmesh.v1.AgentPhase.AGENT_PHASE_PLANNING).build())
        show { HomeScreen(rememberSidebar(), null, { _, _, _ -> }, {}, {}, {}, {}) }
        waitForText("shop-fix sent a plan")
        compose.onNodeWithText("Waiting on you (1)").assertIsDisplayed()
        // Opening the row shows the plan and its Approve.
        compose.onNodeWithText("shop-fix sent a plan").performClick()
        compose.onNodeWithText("Change the typo in README").assertIsDisplayed()
        compose.waitUntil(5_000) { "r1" in rt.state.value.messagesLoaded }
        compose.onNodeWithText("Approve").performClick()
        compose.waitUntil(5_000) { api.approved.isNotEmpty() }
        assertEquals(listOf("w1"), api.approved.toList())
    }

    // ── The live dock ──

    @Test fun `the live dock shows what each live agent does now, the ones that need you first`() {
        api = RecordingApi()
        NotificationsTest.api = api
        val rt = host(
            agent("w1", name = "busy-one", state = AgentState.AGENT_STATE_WORKING, detail = "Bash"),
            agent("w2", name = "stuck-one", state = AgentState.AGENT_STATE_NEEDS_INPUT, needsInput = 1, detail = "Edit"),
            agent("w3", name = "done-one", state = AgentState.AGENT_STATE_EXITED),
        )
        rt.onFrame(messageSent(1, message("m1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, at = 3_000)))
        // A tool call changes the working agent's line without any event.
        rt.onFrame(activity("w1", "Read", at = 8_000))
        var opened: Pair<String, Tab>? = null
        show { AllHostsDockScreen({}) { _, a, t -> opened = a to t } }
        compose.onAllNodesWithTag("tile").assertCountEquals(2)
        compose.onNodeWithText("Working: Read").assertIsDisplayed()
        compose.onNodeWithText("Needs you: Edit").assertIsDisplayed()
        // The card is one clickable node: the line inside it is in the unmerged tree.
        compose.onNodeWithTag("tile-sent", useUnmergedTree = true).assertIsDisplayed()
        compose.onNodeWithText("stuck-one").performClick()
        assertEquals("w2" to Tab.Terminal, opened)
    }

    // ── Model and effort ──

    @Test fun `the model is shown, a switch under way is said, and a tier is chosen in a sheet`() {
        api = RecordingApi().apply { transcript = { _, _, _ -> page(emptyList()) } }
        NotificationsTest.api = api
        val rt = host(worker.toBuilder().setPendingReopen(Reopen.newBuilder().setModel("opus").setEffort("high").setWhy("moved")).build())
        show { AgentScreen(rt, "w1", {}) }
        compose.onNodeWithTag("runs-on").assertIsDisplayed()
        compose.onNodeWithText("claude · sonnet · high").assertIsDisplayed()
        compose.onNodeWithText("Switching to opus (moved)…").assertIsDisplayed()
        compose.onNodeWithTag("change-model").performClick()
        compose.onNodeWithTag("model-sheet").assertIsDisplayed()
        compose.onNodeWithTag("tier-strong").performClick()
        compose.onNodeWithTag("switch").performClick()
        compose.waitUntil(5_000) { api.switches.isNotEmpty() }
        assertEquals(listOf(listOf("w1", "strong", "", "")), api.switches.toList())
        assertEquals(1, api.modelChecks)
    }

    @Test fun `an effort alone, or a model with an effort, and the host's refusal in its own words`() {
        api = RecordingApi().apply { transcript = { _, _, _ -> page(emptyList()) } }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}) }
        compose.onNodeWithTag("change-model").performClick()
        compose.onNodeWithTag("by-Effort").performClick()
        compose.onNodeWithTag("switch").assertIsNotEnabled()
        compose.onNodeWithTag("effort-max").performClick()
        compose.onNodeWithTag("switch").performClick()
        compose.waitUntil(5_000) { api.switches.isNotEmpty() }
        assertEquals(listOf("w1", "", "", "max"), api.switches.last())
        // A refusal keeps the sheet open and says why.
        compose.onNodeWithTag("change-model").performClick()
        api.failSwitch = ApiException(Code.FAILED_PRECONDITION, "[failed_precondition] w1 already runs on sonnet")
        compose.onNodeWithTag("by-Model").performClick()
        compose.onNodeWithTag("model").performTextInput("sonnet")
        compose.onNodeWithTag("switch").performClick()
        waitForTag("switch-error")
        compose.onNodeWithText("w1 already runs on sonnet").assertIsDisplayed()
        compose.onNodeWithTag("model-sheet").assertIsDisplayed()
    }

    // ── Pull requests ──

    @Test fun `a pull request can be held and released, and a refusal is shown`() {
        api = RecordingApi().apply {
            transcript = { _, _, _ -> page(emptyList()) }
            pullRequests = mapOf("r1" to listOf(agentPrs("w1", pr("w1", "https://github.com/o/r/pull/7", 7, checks = "failing"))))
        }
        NotificationsTest.api = api
        val rt = host(worker)
        waitUntilState(rt) { "w1" in it.pullRequests }
        show { AgentScreen(rt, "w1", {}) }
        compose.onNodeWithText("#7").assertIsDisplayed()
        compose.onNodeWithText("open · checks failing").assertIsDisplayed()
        compose.onNodeWithTag("hold").performClick()
        compose.waitUntil(5_000) { api.holds.isNotEmpty() }
        assertEquals(listOf("https://github.com/o/r/pull/7" to false), api.holds.toList())
        // The host's PrChanged event is what shows the hold.
        rt.onFrame(prChanged(2, pr("w1", "https://github.com/o/r/pull/7", 7, held = true)))
        compose.waitUntil(5_000) { compose.onAllNodesWithText("Release").fetchSemanticsNodes().isNotEmpty() }
        api.failHold = ApiException(Code.PERMISSION_DENIED, "[permission_denied] only the user releases a hold")
        compose.onNodeWithText("Release").performClick()
        waitForTag("hold-error")
        compose.onNodeWithText("only the user releases a hold").assertIsDisplayed()
    }

    @Test fun `a read-only device sees the pull request but has no button to hold it`() {
        api = RecordingApi().apply {
            transcript = { _, _, _ -> page(emptyList()) }
            pullRequests = mapOf("r1" to listOf(agentPrs("w1", pr("w1", "https://github.com/o/r/pull/7", 7))))
        }
        NotificationsTest.api = api
        val rt = host(worker, actor = ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE)
        waitUntilState(rt) { "w1" in it.pullRequests }
        show { AgentScreen(rt, "w1", {}) }
        compose.onNodeWithText("#7").assertIsDisplayed()
        compose.onAllNodesWithTag("hold").assertCountEquals0()
    }

    // ── Usage and cost ──

    @Test fun `usage shows the quota bars against the reserve, and the spend with tokens first`() {
        api = RecordingApi().apply {
            usage = GetUsageResponse.newBuilder().setQuotaStop(90).setMinQuota(10).setClaude(
                ClaudeUsage.newBuilder().addAtReserve("five-hour window")
                    .addWindows(ClaudeUsageWindow.newBuilder().setKey("five_hour").setLabel("five-hour window").setUsed(93.0))
                    .addWindows(ClaudeUsageWindow.newBuilder().setKey("seven_day").setLabel("seven-day window").setUsed(40.0)),
            ).build()
            cost = GetCostResponse.newBuilder().setScope("since 7d").setUsd(4.5).setTotalTokens(12_300_000).setTotalRead(40_000_000)
                .addClis(CliCost.newBuilder().setCli("claude").setCalls(40).setInput(1_000_000).setOutput(500_000).setTotal(3_800_000).setBasis("exact").setUsd(4.5)).build()
        }
        NotificationsTest.api = api
        val rt = host(worker)
        show { UsageScreen(rt, {}, quotaEveryMs = 300) }
        waitForTag("quota-warning")
        compose.onNodeWithText("At the reserve: Claude five-hour window").assertIsDisplayed()
        compose.onAllNodesWithTag("quota-bar").assertCountEquals(2)
        waitForTag("cost-tokens")
        compose.onNodeWithText("12.3M tokens").assertIsDisplayed()
        compose.onNodeWithTag("cost-usd").assertIsDisplayed()
        assertEquals(listOf("" to "7d"), api.costCalls.toList())
        // Another span, then Refresh asks the host to read the quotas again.
        compose.onNodeWithTag("span-24h").performClick()
        compose.waitUntil(5_000) { api.costCalls.size >= 2 }
        assertEquals("" to "24h", api.costCalls.last())
        compose.onNodeWithTag("refresh").performClick()
        compose.waitUntil(5_000) { true in api.usageCalls }
        // The timed reads after it don't force the host's cache again.
        compose.waitUntil(5_000) { api.usageCalls.size >= 4 }
        assertEquals(1, api.usageCalls.count { it })
    }

    @Test fun `the spend of one run is asked for by its id`() {
        api = RecordingApi()
        NotificationsTest.api = api
        val rt = host(worker)
        show { UsageScreen(rt, onBack = {}) }
        waitForTag("cost-tokens")
        compose.onNodeWithTag("span-run").performClick()
        compose.onNodeWithText("Fix the typo").performClick()
        compose.waitUntil(5_000) { api.costCalls.any { it.first == "r1" } }
    }

    // ── Settings ──

    @Test fun `key chords are not shown on a phone, with a line saying so`() {
        fun setting(key: String, type: String) = Setting.newBuilder().setKey(key).setSchema(SettingSchema.newBuilder().setType(type).setSummary("s-$key")).build()
        api = RecordingApi().apply { settings = listOf(setting("app.keys.palette", "shortcut"), setting("app.keys.dock", "shortcut"), setting("app.theme", "string")) }
        NotificationsTest.api = api
        val rt = host(worker)
        show { HostSettingsScreen(rt) {} }
        waitForText("app.theme")
        compose.onNodeWithText("app.theme").assertIsDisplayed()
        compose.onAllNodesWithText("app.keys.palette").assertCountEquals0()
        compose.onNodeWithTag("hidden-shortcuts").performScrollTo().assertIsDisplayed()
        compose.onNodeWithText("2 keyboard shortcuts", substring = true).assertIsDisplayed()
    }

    // ── What the round-1 review found ──

    @Test fun `a resync on a connection that stays up lists what waits again`() {
        api = RecordingApi().apply { waitingList = emptyList() }
        NotificationsTest.api = api
        val rt = host(worker)
        waitUntilState(rt) { api.waitingListings >= 1 }
        val before = api.waitingListings
        // The stream resyncs (a snapshot again) with no state flow change in between: the refresher hears it straight from the frame.
        rt.onFrame(snapshot(listOf(run("r1")), listOf(worker)))
        waitUntilState(rt) { api.waitingListings > before }
        val again = api.waitingListings
        rt.onFrame(dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse.newBuilder().setResumed(dev.agentmesh.gen.agentmesh.v1.Resumed.getDefaultInstance()).build())
        waitUntilState(rt) { api.waitingListings > again }
    }

    @Test fun `offline, the model sheet's Switch is off and nothing is kept to be sent later`() {
        api = RecordingApi().apply { transcript = { _, _, _ -> page(emptyList()) } }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}) }
        compose.onNodeWithTag("change-model").performClick()
        compose.onNodeWithTag("tier-strong").performClick()
        compose.onNodeWithTag("switch").assertIsEnabled()
        rt.onState(ConnState.Reconnecting(1, System.currentTimeMillis() + 60_000, "gone"))
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("offline").fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithTag("switch").assertIsNotEnabled()
        compose.onNodeWithTag("model-sheet").assertIsDisplayed()
        // The call itself fails at once when it is made offline (no waiting for a later connection).
        assertTrue(runCatching { rt.connectedApi() }.exceptionOrNull() is NotConnected)
        assertEquals(emptyList<List<String>>(), api.switches.toList())
    }

    @Test fun `offline, a prompt keeps its text and Send waits`() {
        api = RecordingApi().apply { transcript = { _, _, _ -> page(emptyList()) } }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForText("Nothing said yet.")
        compose.onNodeWithTag("prompt").performTextInput("later")
        compose.onNodeWithTag("send").assertIsEnabled()
        rt.onState(ConnState.Reconnecting(1, System.currentTimeMillis() + 60_000, "gone"))
        waitForTag("prompt-offline")
        compose.onNodeWithTag("send").assertIsNotEnabled()
        compose.onNodeWithText("later").assertIsDisplayed()
        assertEquals(emptyList<Pair<String, String>>(), api.prompts.toList())
    }

    @Test fun `offline, a PR's Hold merge is off`() {
        api = RecordingApi().apply {
            transcript = { _, _, _ -> page(emptyList()) }
            pullRequests = mapOf("r1" to listOf(agentPrs("w1", pr("w1", "https://github.com/o/r/pull/7", 7))))
        }
        NotificationsTest.api = api
        val rt = host(worker)
        waitUntilState(rt) { "w1" in it.pullRequests }
        show { AgentScreen(rt, "w1", {}) }
        compose.onNodeWithTag("hold").assertIsEnabled()
        rt.onState(ConnState.Reconnecting(1, System.currentTimeMillis() + 60_000, "gone"))
        compose.waitUntil(5_000) { runCatching { compose.onNodeWithTag("hold").assertIsNotEnabled() }.isSuccess }
        assertEquals(emptyList<Pair<String, Boolean>>(), api.holds.toList())
    }

    /** [n] entries of three lines each, tagged `prefix-001`… so one is told from another by substring. */
    private fun longTail(n: Int, prefix: String) = (1..n).map { i ->
        val tag = "$prefix-${"%03d".format(i)}"
        entry("$prefix$i", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "$tag\nsecond line of $tag\nthird line of $tag")
    }

    private fun waitForTextContaining(t: String) = compose.waitUntil(5_000) { compose.onAllNodesWithText(t, substring = true).fetchSemanticsNodes().isNotEmpty() }
    private fun textCount(t: String) = compose.onAllNodesWithText(t, substring = true).fetchSemanticsNodes().size

    @Test fun `a long conversation opens at its end`() {
        api = RecordingApi().apply { transcript = { _, _, back -> if (back) page(longTail(80, "msg")) else page(emptyList()) } }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForTextContaining("msg-080")
        compose.onNodeWithText("msg-080", substring = true).assertIsDisplayed()
        assertEquals(0, textCount("msg-001"))
    }

    @Test fun `Load earlier keeps the reader's place`() {
        api = RecordingApi().apply {
            transcript = { _, off, back ->
                when {
                    back && off != -1L -> page(longTail(60, "old"), atStart = true).toBuilder().setEndOffset(50).build()
                    back -> page(longTail(60, "new"), atStart = false).toBuilder().setStartOffset(50).build()
                    else -> page(emptyList())
                }
            }
        }
        NotificationsTest.api = api
        val rt = host(worker)
        show { AgentScreen(rt, "w1", {}, Tab.Chat) }
        waitForTextContaining("new-060")
        // Read back to the top of what is held, then ask for earlier entries.
        compose.onNodeWithTag("chat").performScrollToIndex(0)
        compose.onNodeWithTag("load-earlier").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("load-earlier").fetchSemanticsNodes().isEmpty() }
        // What the reader was looking at (the first held entry) is still on screen, and the earlier ones are above it.
        assertTrue(textCount("new-001") > 0)
        assertEquals(0, textCount("old-001"))
    }

    @Test fun `a long plan in the waiting list can be shown whole`() {
        val body = (1..40).joinToString("\n") { "Step $it of the plan" }
        val plan = message("p1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = body, at = 10_000)
        api = RecordingApi().apply { waitingList = listOf(waiting(WaitingKind.WAITING_KIND_PLAN, "w1", message = plan)) }
        NotificationsTest.api = api
        val rt = host(agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead"), agent("w1", parent = "lead", name = "shop-fix"))
        show { HomeScreen(rememberSidebar(), null, { _, _, _ -> }, {}, {}, {}, {}) }
        waitForText("shop-fix sent a plan")
        compose.onNodeWithText("shop-fix sent a plan").performClick()
        compose.onNodeWithText("Show all").performScrollTo().performClick()
        compose.onNodeWithText("Show less").assertExists()
    }

    @Test fun `a read-only device sees the plan that waits but has no form to answer it`() {
        val plan = message("p1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "Change the typo", at = 10_000)
        api = RecordingApi().apply { waitingList = listOf(waiting(WaitingKind.WAITING_KIND_PLAN, "w1", message = plan)); pages = mapOf("r1" to listOf(listOf(plan))) }
        NotificationsTest.api = api
        val rt = host(agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead"), worker, actor = ActorClass.ACTOR_CLASS_READ_ONLY_DEVICE)
        show { AgentScreen(rt, "w1", {}, Tab.Messages) }
        waitForText("waiting for you")
        compose.onAllNodesWithText("Approve").assertCountEquals(0)
    }

    @Test fun `a forced quota read happens once per press, not again after a reconnect`() {
        api = RecordingApi()
        NotificationsTest.api = api
        val rt = host(worker)
        show { UsageScreen(rt, onBack = {}, quotaEveryMs = 60_000) }
        waitForTag("refresh")
        compose.waitUntil(5_000) { api.usageCalls.isNotEmpty() }
        compose.onNodeWithTag("refresh").performClick()
        compose.waitUntil(5_000) { true in api.usageCalls }
        assertEquals(1, api.usageCalls.count { it })
        val before = api.usageCalls.size
        // The connection drops and returns: the quota is read again, without forcing the host's cache.
        rt.onState(ConnState.Reconnecting(1, System.currentTimeMillis() + 60_000, "gone"))
        compose.waitForIdle()
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        compose.waitUntil(5_000) { api.usageCalls.size > before }
        assertEquals(1, api.usageCalls.count { it })
    }

    @Test fun `a spend error doesn't outlive the span it was for, and the per-agent table can be asked for`() {
        api = RecordingApi().apply { failCost = ApiException(Code.UNAVAILABLE, "[unavailable] gh missing") }
        NotificationsTest.api = api
        val rt = host(worker)
        show { UsageScreen(rt, onBack = {}) }
        waitForTag("cost-error")
        api.failCost = null
        compose.onNodeWithTag("span-24h").performClick()
        compose.waitUntil(5_000) { compose.onAllNodesWithTag("cost-error").fetchSemanticsNodes().isEmpty() }
        compose.onNodeWithTag("per-agent").performClick()
        compose.waitUntil(5_000) { true in api.costAgents }
    }
}

private fun androidx.compose.ui.test.SemanticsNodeInteractionCollection.assertCountEquals0() = assertCountEquals(0)
