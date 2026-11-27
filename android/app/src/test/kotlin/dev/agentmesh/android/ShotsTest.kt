package dev.agentmesh.android

import android.app.Application
import android.graphics.Bitmap
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.test.captureToImage
import androidx.compose.ui.test.hasTestTag
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onRoot
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performScrollToIndex
import androidx.compose.ui.test.performTextInput
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.theme.MeshTheme
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AgentScreen
import dev.agentmesh.android.ui.HomeScreen
import dev.agentmesh.android.ui.SidebarActions
import dev.agentmesh.android.ui.SidebarContent
import dev.agentmesh.android.ui.rememberSidebar
import dev.agentmesh.android.ui.HostsScreen
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse
import dev.agentmesh.gen.agentmesh.v1.TranscriptEntry
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import org.junit.After
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode
import java.io.File

/**
 * The screens as pictures, for whoever changes the UI (nobody here has a phone or an emulator: `android/CLAUDE.md`). Each
 * test renders a screen of a seeded host (two runs, workers, reviewers, an asker, a plan and a task waiting) with
 * Robolectric's native graphics and writes a PNG to `app/build/shots/` (or `$SHOTS_DIR`), on a 411x891dp phone, in a
 * dark theme and a light one. They are for looking at, before and after a change, not goldens: a screenshot shifts with
 * the machine's text rendering, so nothing here compares pixels. What a screen does is asserted by the other Compose tests.
 */
@RunWith(RobolectricTestRunner::class)
@GraphicsMode(GraphicsMode.Mode.NATIVE)
@Config(sdk = [36], qualifiers = "w411dp-h891dp-xxhdpi")
class ShotsTest {
    @get:Rule val compose = createComposeRule()
    @get:Rule val tmp = TemporaryFolder()

    private val out = File(System.getenv("SHOTS_DIR") ?: "build/shots").also { it.mkdirs() }
    private fun theme(file: String): MeshTheme = Themes.resolve(Themes.parse(File("../../shared/themes/$file.json").readText()))
    private val dark = theme("dracula")
    private val light = theme("catppuccin-latte")

    /** A host that answers the chat's transcript and nothing else. */
    private fun api(): MeshApi = RecordingApi().apply {
        transcript = { _, _, _ -> transcript() }
        // Two prompts the lead's host holds: one for its turn, one for a draft left in its box.
        queued["lead"] = listOf(queuedPrompt("p1", "lead", "Then bump the changelog.", held = "working"), queuedPrompt("p2", "lead", "And tag the release once CI is green.", at = 2_000, held = "draft"))
    }

    private fun transcript(): ReadTranscriptResponse {
        fun e(id: String, kind: TranscriptKind, text: String, tool: String = "", input: String = "") =
            TranscriptEntry.newBuilder().setId(id).setKind(kind).setText(text).setTool(tool).setToolInput(input).setToolUseId(id).build()
        val entries = listOf(
            e("1", TranscriptKind.TRANSCRIPT_KIND_USER, "Fix the flaky retry test in billing, then:\n1. run the suite\n2. open a PR"),
            e("2", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "I'll look at the **retry** logic first.\n\n# Findings\n\n- `billing/retry.go` backs off with no jitter\n- the test sleeps a fixed 50 ms\n\n```go\nfunc backoff(n int) time.Duration {\n    return time.Duration(n) * 50 * time.Millisecond\n}\n```"),
            e("3", TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL, "", "Bash", "go test ./billing/..."),
            e("4", TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL, "", "Edit", "billing/retry.go"),
            e("5", TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, "Done: the test passes 200 times in a row. PR #42 is open."),
        )
        return ReadTranscriptResponse.newBuilder().setSupported(true).setSessionId("s1").addAllEntries(entries).setStartOffset(0).setEndOffset(100).setAtStart(true).build()
    }

    private val started = mutableListOf<HostRuntime>()

    @After fun stopRuntimes() { started.forEach { it.stop() } }

    private fun runtime(): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("wsl", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = HostRuntime(host.id, repo, { _, _ -> api() }, CoroutineScope(Dispatchers.Unconfined))
        // Started, so the runtime has built its client (the chat reads through it); its handshake never answers, so the state is the test's.
        rt.start()
        started += rt
        seed(rt)
        return rt
    }

    /** Two runs, workers, reviewers and an asker, a plan and a task: what the screens show. */
    private fun seed(rt: HostRuntime) {
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        fun a(id: String, name: String, state: AgentState, parent: String = "", role: AgentRole = AgentRole.AGENT_ROLE_WORKER, runId: String = "r1", repo: String = "/home/u/agent-mesh", kind: String = "claude", model: String = "claude-opus-5-5", needs: Int = 0, detail: String = "") =
            agent(id, runId = runId, role = role, parent = parent, state = state, name = name, at = 2_000, needsInput = needs, detail = detail).toBuilder().setRepo(repo).setKind(kind).setModel(model).setTerminalId("t-$id").build()
        val agents = listOf(
            a("lead", "agent-mesh-lead", AgentState.AGENT_STATE_WORKING, role = AgentRole.AGENT_ROLE_ROOT),
            a("w1", "agent-mesh-sidebar", AgentState.AGENT_STATE_NEEDS_INPUT, parent = "lead", needs = 1, detail = "Bash"),
            a("w2", "agent-mesh-chat", AgentState.AGENT_STATE_WORKING, parent = "lead", kind = "agy", model = "gemini-3-pro"),
            a("rv1", "sidebar-review-1", AgentState.AGENT_STATE_EXITED, parent = "w1", role = AgentRole.AGENT_ROLE_REVIEWER),
            a("rv2", "sidebar-review-2", AgentState.AGENT_STATE_RETIRED, parent = "w1", role = AgentRole.AGENT_ROLE_REVIEWER, kind = "opencode", model = "deepseek-v4-pro"),
            a("as1", "chat-asker", AgentState.AGENT_STATE_IDLE, parent = "w2", role = AgentRole.AGENT_ROLE_ASKER, kind = "copilot", model = "gpt-5"),
            a("lead2", "billing-lead", AgentState.AGENT_STATE_IDLE, role = AgentRole.AGENT_ROLE_ROOT, runId = "r2", repo = "/home/u/billing"),
            a("w3", "billing-retries", AgentState.AGENT_STATE_EXITED, parent = "lead2", runId = "r2", repo = "/home/u/billing"),
        )
        rt.onFrame(snapshot(listOf(run("r1", "Desktop UI overhaul"), run("r2", "Billing retries")), agents))
        rt.onFrame(messageSent(1, message("m1", from = "w2", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "# Plan\n\n1. Port the sidebar\n2. **Tests** first", at = 3_000)))
        rt.onFrame(messageSent(2, message("m2", from = "lead", to = "w2", type = MessageType.MESSAGE_TYPE_TASK, body = "Build the chat view. See `plans/chat.md`.", at = 2_500)))
    }

    /** The app with the seeded host paired, as the drawer and home read it. */
    private fun seeded(): AppContainer {
        val ctx = ApplicationProvider.getApplicationContext<Application>()
        File(ctx.filesDir, "hosts.json").delete()
        val c = AppContainer(ctx, Plain, newApi = { _, _ -> api() })
        val h = c.hosts.add("wsl", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val deadline = System.currentTimeMillis() + 5_000
        while (c.runtimes.value[h.id] == null && System.currentTimeMillis() < deadline) Thread.sleep(20)
        val rt = c.runtimes.value.getValue(h.id)
        started += rt
        seed(rt)
        return rt.let { c }
    }

    private fun container() = AppContainer(ApplicationProvider.getApplicationContext<Application>(), Plain, newApi = { _, _ -> api() })

    private fun shot(name: String) {
        // Markdown is parsed, then its code highlighted, each off the main thread: let both finish before the picture.
        repeat(2) { Thread.sleep(500); compose.waitForIdle() }
        val bmp = compose.onRoot().captureToImage().asAndroidBitmap()
        File(out, "$name.png").outputStream().use { bmp.compress(Bitmap.CompressFormat.PNG, 100, it) }
    }

    private fun both(name: String, content: @androidx.compose.runtime.Composable () -> Unit, ready: () -> Unit = {}, c: AppContainer = container()) {
        // The same screen in each theme: one composition, the theme swapped, so a state is built once.
        var t by mutableStateOf(dark)
        compose.setContent { CompositionLocalProvider(LocalContainer provides c) { AgentMeshTheme(t) { content() } } }
        ready()
        shot("$name-dark")
        t = light
        shot("$name-light")
    }

    private val noActions = SidebarActions({ _, _, _ -> }, {}, {}, {}, {}, { _, _ -> }, { _, _ -> }, {})

    @Test fun home() { val c = seeded(); both("home", { HomeScreen(rememberSidebar(), {}, { _, _, _ -> }, {}, {}, {}, {}) }, c = c) }

    /** The Activity list: what the host decided to tell the user, the ones for them marked. */
    @Test fun activity() {
        val c = seeded()
        val rt = c.runtimes.value.values.first()
        val now = System.currentTimeMillis()
        rt.onFrame(userNotice(11, "plan", agentId = "w2", title = "agent-mesh-chat sent a plan", body = "Port the sidebar, then the chat view", at = now - 600_000))
        rt.onFrame(userNotice(12, "report", agentId = "w1", forUser = false, title = "agent-mesh-sidebar reported done", body = "Sidebar ported, tests pass", at = now - 300_000))
        rt.onFrame(userNotice(13, "input", agentId = "w1", tab = "terminal", title = "agent-mesh-sidebar needs you", body = "Bash", at = now - 120_000))
        rt.onFrame(userNotice(14, "ready", agentId = "lead", title = "PR #42 is ready to merge", body = "Desktop UI overhaul", at = now - 30_000))
        both("activity", { dev.agentmesh.android.ui.ActivityScreen({}, { _, _, _ -> }) }, c = c)
    }

    @Test fun drawer() {
        val c = seeded()
        both("drawer", { androidx.compose.material3.Surface { SidebarContent(rememberSidebar(), "x" to "w2", noActions) } }, c = c)
    }

    /** A worker's chat: its tree strip, the plan it sent as a card, the reply with code, a tool run, the composer. */
    @Test fun agentChat() {
        val rt = runtime()
        both("agent-chat", { AgentScreen(rt, "w2", {}, Tab.Chat) }, ready = { compose.waitUntil(5_000) { compose.onAllNodes(hasTestTag("chat-assistant")).fetchSemanticsNodes().isNotEmpty() } })
    }

    /** The lead's chat, with a worker that needs the user in its strip. */
    @Test fun agentLeadChat() {
        val rt = runtime()
        both("agent-lead-chat", { AgentScreen(rt, "lead", {}, Tab.Chat) }, ready = { compose.waitUntil(5_000) { compose.onAllNodes(hasTestTag("chat-assistant")).fetchSemanticsNodes().isNotEmpty() } })
    }

    /** The composer while it is used: a list being typed, the formatting row above the keyboard, the queue and Stop (the lead works). */
    @Test fun agentComposer() {
        val rt = runtime()
        both("agent-composer", { AgentScreen(rt, "lead", {}, Tab.Chat) }, ready = {
            compose.waitUntil(5_000) { compose.onAllNodes(hasTestTag("chat-assistant")).fetchSemanticsNodes().isNotEmpty() }
            // The Markdown above grows after the list was placed: the end is looked at again once it has.
            compose.onNodeWithTag("chat").performScrollToIndex(7)
            compose.waitUntil(5_000) { compose.onAllNodes(hasTestTag("queued-p1")).fetchSemanticsNodes().isNotEmpty() }
            compose.onNodeWithTag("prompt").performClick()
            compose.onNodeWithTag("prompt").performTextInput("Also:\n- the retry docs")
            compose.onNodeWithTag("prompt").performTextInput("\n")
            compose.waitForIdle()
        })
    }

    /** The details sheet (it is a window of its own: the picture shows the screen behind it when the capture can't reach it). */
    @Test fun agentDetails() {
        val rt = runtime()
        both("agent-details", { AgentScreen(rt, "w1", {}, Tab.Chat) }, ready = { compose.onNodeWithTag("details-open").performClick(); compose.waitForIdle() })
    }

    @Test fun settings() { val c = container(); both("settings", { dev.agentmesh.android.ui.settings.DeviceSettingsScreen({}) }, c = c) }

    @Test fun hosts() {
        val ctx = ApplicationProvider.getApplicationContext<Application>()
        File(ctx.filesDir, "hosts.json").delete()
        val c = AppContainer(ctx, Plain)
        c.hosts.add("wsl", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        var t by mutableStateOf(dark)
        compose.setContent { CompositionLocalProvider(LocalContainer provides c) { AgentMeshTheme(t) { HostsScreen({}, {}, {}, {}, {}) } } }
        compose.waitUntil(5_000) { compose.onAllNodes(androidx.compose.ui.test.hasText("wsl")).fetchSemanticsNodes().isNotEmpty() }
        shot("hosts-dark")
        t = light
        shot("hosts-light")
    }
}
