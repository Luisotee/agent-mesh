package dev.agentmesh.android

import dev.agentmesh.android.net.ConnectMeshApi
import dev.agentmesh.android.net.DefaultRouteConnector
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.chat.fromTail
import dev.agentmesh.android.term.TermClient
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.term.TermState
import kotlinx.coroutines.CoroutineScope
import dev.agentmesh.android.chat.toBlocks
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test

/**
 * The calls of the chat, the dock, the buckets and the usage screen against a real meshd (the fake `claude` in its
 * PTYs, an isolated HOME and state dir, no real CLI, no network): the generated client and the host agree on the wire.
 * `CheckModels` is not called: it asks the CLIs the host has for their models.
 */
class ParityE2ETest {
    private var daemon: DaemonHarness? = null

    @Before fun setUp() {
        assumeTrue("no Go to build the daemon with, or SKIP_E2E is set", DaemonHarness.available)
        daemon = DaemonHarness.create().also { it.start(transcripts = true) }
    }

    @After fun tearDown() { daemon?.close() }

    private fun api() = ConnectMeshApi(DefaultRouteConnector().connect(Route.Direct(daemon!!.url)) { daemon!!.token })

    @Test fun `a prompt typed into a real agent shows in its transcript, and the host answers the rest`() = runBlocking {
        val d = daemon!!
        val repo = d.repo("shop-api")
        val run = d.ctl("start-run", "--title", "chat").getValue("run").jsonObject.getValue("id").jsonPrimitive.content
        val agent = withContext(Dispatchers.IO) {
            d.ctl("spawn", "--run-id", run, "--repo", repo.path, "--slug", "fix", "--kind", "claude", "--role", "worker", "--model", "haiku")
                .getValue("agent").jsonObject.getValue("id").jsonPrimitive.content
        }
        withTimeout(30_000) {
            while (withContext(Dispatchers.IO) { d.ctl("agent", "--agent", agent).getValue("agent").jsonObject.getValue("state").jsonPrimitive.content } != "AGENT_STATE_IDLE") delay(100)
        }
        val api = api()
        api.promptAgent(agent, "say hello")
        // The turn shows in the record the chat reads: what the user typed, then the reply.
        val kinds = withTimeout(30_000) {
            var t = fromTail(api.readTranscript(agent, -1, true))
            while (t.entries.none { it.kind == TranscriptKind.TRANSCRIPT_KIND_USER && "say hello" in it.text }) { delay(200); t = fromTail(api.readTranscript(agent, -1, true)) }
            toBlocks(t.entries).map { it::class.simpleName }
        }
        assertTrue("got $kinds", "User" in kinds)

        // What waits on the user, the agents, the PRs, the quotas and the spend: each answers, and in the shapes the app reads.
        assertEquals(emptyList<String>(), api.listWaiting().map { it.agentId })
        assertTrue(api.listAgents().any { it.id == agent })
        assertEquals(emptyList<String>(), api.listPullRequests(run).flatMap { it.pullRequestsList }.map { it.url })
        api.getUsage(refresh = false)
        val cost = api.getCost(since = "24h")
        assertTrue(cost.scope.isNotEmpty())
        api.getCost(runId = run)
        Unit
    }

    @Test fun `typing from the phone reaches a real agent's terminal, without ever resizing it`() = runBlocking {
        val d = daemon!!
        val repo = d.repo("shop-api")
        val run = d.ctl("start-run", "--title", "typing").getValue("run").jsonObject.getValue("id").jsonPrimitive.content
        val agent = withContext(Dispatchers.IO) {
            d.ctl("spawn", "--run-id", run, "--repo", repo.path, "--slug", "fix", "--kind", "claude", "--role", "worker", "--model", "haiku")
                .getValue("agent").jsonObject.getValue("id").jsonPrimitive.content
        }
        withTimeout(30_000) {
            while (withContext(Dispatchers.IO) { d.ctl("agent", "--agent", agent).getValue("agent").jsonObject.getValue("state").jsonPrimitive.content } != "AGENT_STATE_IDLE") delay(100)
        }
        val dispatcher = Dispatchers.Default.limitedParallelism(1)
        val screen = TermScreen()
        val client = TermClient(CoroutineScope(dispatcher), agent, { api() }, screen, input = true)
        client.start()
        try {
            withTimeout(30_000) { while (!client.typing.value) delay(50) }
            val before = withContext(dispatcher) { screen.cols to screen.rows }
            assertTrue(client.type("typed from the phone".toByteArray()))
            withTimeout(20_000) { while ("typed from the phone" !in withContext(dispatcher) { screen.visibleText() }) delay(50) }
            // The terminal is the size it was: a phone that attaches with no size never moves it.
            assertEquals(before, withContext(dispatcher) { screen.cols to screen.rows })
            assertEquals(TermState.Live, client.state.value)
        } finally {
            client.stop()
        }
    }

    @Test fun `a run and its agent started from the phone's calls appear on the real host`() = runBlocking {
        val d = daemon!!
        val repo = d.repo("shop-api")
        val api = api()
        val run = api.startRun("from the phone")
        assertEquals("from the phone", run.title)
        val spawned = api.spawnAgent(run.id, repo.path, "fix", dev.agentmesh.gen.agentmesh.v1.AgentRole.AGENT_ROLE_WORKER, "claude", tier = "", model = "haiku", task = "")
        assertEquals(run.id, spawned.agent.runId)
        assertEquals("fix", spawned.agent.name.substringAfterLast('-'))
        // The host lists it, and the repo is one it knows now.
        assertTrue(api.listAgents().any { it.id == spawned.agent.id })
        assertTrue(api.listProjects().any { it.path.endsWith("shop-api") })
        Unit
    }

    @Test fun `a shell opened from the phone runs on the real host, at the phone's size, takes what is typed and ends when closed`() = runBlocking {
        val d = daemon!!
        val repo = d.repo("shop-api")
        // The host opens a shell only in a folder it knows: one an agent worked in.
        val run = d.ctl("start-run", "--title", "shell").getValue("run").jsonObject.getValue("id").jsonPrimitive.content
        withContext(Dispatchers.IO) { d.ctl("spawn", "--run-id", run, "--repo", repo.path, "--slug", "fix", "--kind", "claude", "--role", "worker", "--model", "haiku") }
        val api = api()
        val shell = api.openShell(repo.path)
        assertTrue(api.listShells().any { it.id == shell.id && it.repo.endsWith("shop-api") })
        val dispatcher = Dispatchers.Default.limitedParallelism(1)
        val screen = TermScreen()
        val client = TermClient(CoroutineScope(dispatcher), shell.id, { api }, screen, input = true, shell = true, size = { 70 to 20 })
        client.start()
        try {
            withTimeout(30_000) { while (!client.typing.value) delay(50) }
            // The phone attached with its own size, and the shell has it (it is the only client).
            assertEquals(70 to 20, withContext(dispatcher) { screen.cols to screen.rows })
            // The sum is computed by the shell: its answer is on screen, which the typed line (the multiplication) is not.
            assertTrue(client.type("echo phone-shell-\$((6*7))\r".toByteArray()))
            val seen = { runBlocking { withContext(dispatcher) { screen.visibleText() } } }
            try { withTimeout(20_000) { while ("phone-shell-42" !in withContext(dispatcher) { screen.visibleText() }) delay(50) } }
            catch (e: kotlinx.coroutines.TimeoutCancellationException) { throw AssertionError("the shell's answer never came; its screen:\n${seen()}") }
            // The keyboard came up: the view is shorter, and the terminal follows.
            assertTrue(client.resize(70, 12))
            withTimeout(20_000) { while (withContext(dispatcher) { screen.rows } != 12) delay(50) }
            api.closeShell(shell.id)
            withTimeout(20_000) { while (client.state.value != TermState.Ended && client.state.value !is TermState.Exited) delay(50) }
            assertTrue(api.listShells().none { it.id == shell.id })
        } finally {
            client.stop()
        }
    }
}
