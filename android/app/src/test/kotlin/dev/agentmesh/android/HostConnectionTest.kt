package dev.agentmesh.android

import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.ConnectionSink
import dev.agentmesh.android.net.HostConnection
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.PROTOCOL_VERSION
import dev.agentmesh.android.net.RelayRefused
import dev.agentmesh.android.net.RouteException
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.store.Frame
import dev.agentmesh.gen.agentmesh.v1.Cursor
import dev.agentmesh.gen.agentmesh.v1.End
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.Setting
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.receiveAsFlow
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.currentTime
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

private fun helloResponse(version: Int = PROTOCOL_VERSION, daemon: String = "d1") =
    HelloResponse.newBuilder().setDaemonId(daemon).setInstanceId("i1").setVersion("test").setProtocolVersion(version).build()

/** A host that answers as the test says: one hello behaviour and one event channel per attempt. */
private class FakeApi : MeshApi {
    var hellos = 0
    var subscribes = 0
    val cursors = mutableListOf<Cursor?>()
    var onHello: suspend (Int) -> HelloResponse = { helloResponse() }
    val streams = ArrayDeque<Channel<SubscribeEventsResponse>>()

    override suspend fun hello(): HelloResponse = onHello(++hellos)

    override fun subscribe(cursor: Cursor?): Flow<SubscribeEventsResponse> {
        subscribes++
        cursors += cursor
        val ch = streams.removeFirstOrNull() ?: Channel(Channel.UNLIMITED)
        return ch.receiveAsFlow()
    }

    override suspend fun listMessages(runId: String, afterId: String): dev.agentmesh.gen.agentmesh.v1.ListMessagesResponse = error("unused")
    override suspend fun sendMessage(to: String, type: dev.agentmesh.gen.agentmesh.v1.MessageType, body: String): dev.agentmesh.gen.agentmesh.v1.Message = error("unused")
    override suspend fun approveAgent(agent: String): dev.agentmesh.gen.agentmesh.v1.Message = error("unused")
    override suspend fun openTerminal(agent: String, input: Boolean): String = error("unused")
    override fun connectTerminal(path: String): dev.agentmesh.android.net.TerminalSocket = error("unused")
    override suspend fun listSettings(prefix: String): List<Setting> = emptyList()
    override suspend fun setSetting(key: String, value: com.google.protobuf.Value): Setting = error("unused")
    override suspend fun resetSetting(key: String): Setting = error("unused")
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

private class Sink(private val scope: TestScope) : ConnectionSink {
    val states = mutableListOf<ConnState>()
    val frames = mutableListOf<Frame>()
    val logs = mutableListOf<String>()
    var cursor: Cursor? = null
    override fun onState(state: ConnState) { states += state }
    override fun onHello(hello: HelloResponse) {}
    override fun onFrame(frame: Frame) { frames += frame; if (frame.hasEvent()) cursor = Cursor.newBuilder().setSeq(frame.event.seq).setGeneration("g1").build() }
    override fun cursor() = cursor
    override fun log(line: String) { logs += line }
    val last get() = states.last()
}

@OptIn(ExperimentalCoroutinesApi::class)
class HostConnectionTest {
    private fun TestScope.conn(api: () -> MeshApi, sink: Sink, heartbeat: Long = 15_000, routes: Int = 1) =
        HostConnection(backgroundScope, api, sink, now = { currentTime }, random = { 1.0 }, heartbeatMs = heartbeat, helloTimeoutMs = 10_000, baseMs = 500, maxMs = 15_000, routeCount = { routes })

    private fun frameEvent(seq: Long) = SubscribeEventsResponse.newBuilder().setEvent(dev.agentmesh.gen.agentmesh.v1.Event.newBuilder().setSeq(seq).setGeneration("g1")).build()

    @Test fun `it is ready only once hello answers, not when the attempt starts`() = runTest {
        val api = FakeApi()
        val gate = CompletableDeferred<HelloResponse>()
        api.onHello = { gate.await() }
        val sink = Sink(this)
        conn({ api }, sink).start()
        runCurrent()
        assertEquals(listOf<ConnState>(ConnState.Connecting), sink.states)
        gate.complete(helloResponse())
        runCurrent()
        assertTrue(sink.last is ConnState.Ready)
        assertEquals(1, api.subscribes)
    }

    @Test fun `frames reach the sink and a reconnect resumes from the last cursor`() = runTest {
        val api = FakeApi()
        val first = Channel<SubscribeEventsResponse>(Channel.UNLIMITED)
        api.streams += first
        val sink = Sink(this)
        conn({ api }, sink).start()
        runCurrent()
        first.trySend(frameEvent(1)); first.trySend(frameEvent(2)); runCurrent()
        assertEquals(2, sink.frames.size)
        first.close(ApiException(Code.UNAVAILABLE, "connection reset"))
        advanceTimeBy(600); runCurrent()
        assertEquals(listOf(null, 2L), api.cursors.map { it?.seq })
    }

    @Test fun `the wait grows while a host accepts and then drops us, and resets only after a heartbeat is acknowledged`() = runTest {
        val api = FakeApi()
        val sink = Sink(this)
        // Each stream fails the moment it is opened: hello answers, no heartbeat ever gets to run.
        repeat(3) { api.streams += Channel<SubscribeEventsResponse>(Channel.UNLIMITED).also { it.close(ApiException(Code.UNAVAILABLE, "dropped")) } }
        // The fourth attempt stays up past one heartbeat.
        val survivor = Channel<SubscribeEventsResponse>(Channel.UNLIMITED)
        api.streams += survivor
        conn({ api }, sink).start()
        runCurrent()
        advanceTimeBy(500 + 1_000 + 2_000 + 100)
        val retries = sink.states.filterIsInstance<ConnState.Reconnecting>()
        assertEquals(listOf(1, 2, 3), retries.map { it.attempt })
        assertEquals(listOf(500L, 1_500L, 3_500L), retries.map { it.retryAtMs })
        // Once a heartbeat was acknowledged, the next failure starts from the base again.
        advanceTimeBy(20_000)
        survivor.close(ApiException(Code.UNAVAILABLE, "dropped later"))
        runCurrent()
        assertEquals(1, sink.states.filterIsInstance<ConnState.Reconnecting>().last().attempt)
    }

    @Test fun `a relay auth refusal stops a host with one route at once`() = runTest {
        val api = FakeApi()
        api.onHello = { throw ApiException(Code.UNAVAILABLE, "x", RelayRefused("auth", "the relay refused this device's client token")) }
        val sink = Sink(this)
        conn({ api }, sink).start(); runCurrent()
        val stop = sink.last as ConnState.Stopped
        assertEquals(ConnState.Reason.Auth, stop.reason)
        assertTrue(stop.message, "Pair with the host again" in stop.message)
    }

    @Test fun `a relay auth refusal keeps trying the other routes and stops only when every route was refused in a row`() = runTest {
        val api = FakeApi()
        api.onHello = { throw ApiException(Code.UNAVAILABLE, "x", RelayRefused("auth", "the relay refused this device's client token")) }
        val sink = Sink(this)
        conn({ api }, sink, routes = 2).start(); runCurrent()
        // The first refusal: another route is next, after the backoff.
        val retry = sink.last as ConnState.Reconnecting
        assertTrue(retry.error, "relay refused" in retry.error && "pair with the host again" in retry.error)
        advanceTimeBy(600); runCurrent()
        assertEquals(2, api.hellos)
        assertEquals(ConnState.Reason.Auth, (sink.last as ConnState.Stopped).reason)
    }

    @Test fun `a refusal, then a connection that works, then a refusal does not stop a two-route host`() = runTest {
        val api = FakeApi()
        val refuse = ApiException(Code.UNAVAILABLE, "x", RelayRefused("auth", "refused"))
        val first = Channel<SubscribeEventsResponse>(Channel.UNLIMITED)
        api.streams += first
        api.onHello = { n -> if (n == 1 || n == 3) throw refuse else helloResponse() }
        val sink = Sink(this)
        conn({ api }, sink, routes = 2).start(); runCurrent()
        advanceTimeBy(600); runCurrent()
        assertTrue(sink.last is ConnState.Ready) // the second route works
        first.close(ApiException(Code.UNAVAILABLE, "dropped")); runCurrent()
        advanceTimeBy(1_000); runCurrent()
        assertTrue("the refusal after a Ready is the first of a new streak", sink.last is ConnState.Reconnecting)
    }

    @Test fun `a runtime whose relay route is refused ends up on its direct route instead of stopping`() = runTest {
        val dir = java.nio.file.Files.createTempDirectory("hosts").toFile()
        val repo = dev.agentmesh.android.hosts.HostRepository(dir.resolve("hosts.json"), Plain)
        val fp = "ab".repeat(32)
        val host = repo.add(
            "Home", listOf(dev.agentmesh.android.net.Route.Relay("relay:8443", "h1", "amr_stale", fp), dev.agentmesh.android.net.Route.Direct("https://lan:1", fp)), "amt_t",
        )
        val refusing = object : MeshApi by FakeApi() {
            override suspend fun hello(): HelloResponse = throw ApiException(Code.UNAVAILABLE, "x", RelayRefused("auth", "the relay refused this device's client token"))
        }
        val working = FakeApi()
        val rt = dev.agentmesh.android.HostRuntime(host.id, repo, { route, _ -> if (route is dev.agentmesh.android.net.Route.Relay) refusing else working }, backgroundScope, now = { currentTime }, random = { 1.0 })
        rt.start(); runCurrent()
        advanceTimeBy(2_000); runCurrent()
        assertTrue(rt.state.value.conn.toString(), rt.state.value.conn is ConnState.Ready)
    }

    @Test fun `a refused token stops the retries, and a nudge tries again`() = runTest {
        val api = FakeApi()
        api.onHello = { n -> if (n == 1) throw ApiException(Code.UNAUTHENTICATED, "bad token") else helloResponse() }
        val sink = Sink(this)
        val c = conn({ api }, sink)
        c.start()
        runCurrent()
        assertEquals(ConnState.Stopped(ConnState.Reason.Auth, "bad token"), sink.last)
        advanceTimeBy(120_000)
        assertEquals("no retry on its own", 1, api.hellos)
        c.nudge(); runCurrent()
        assertTrue(sink.last is ConnState.Ready)
        assertEquals(2, api.hellos)
    }

    @Test fun `an End frame reconnects at once, without growing the wait`() = runTest {
        val api = FakeApi()
        val s1 = Channel<SubscribeEventsResponse>(Channel.UNLIMITED)
        api.streams += s1
        val sink = Sink(this)
        conn({ api }, sink).start()
        runCurrent()
        advanceTimeBy(16_000)
        s1.trySend(SubscribeEventsResponse.newBuilder().setEnd(End.newBuilder().setReason("shutting down")).build())
        runCurrent()
        assertEquals(2, api.subscribes)
        assertTrue(sink.logs.any { "shutting down" in it })
        val retry = sink.states.filterIsInstance<ConnState.Reconnecting>().single()
        assertEquals(0, retry.attempt)
    }

    @Test fun `an End frame that comes at once, again and again, backs off like any drop`() = runTest {
        val api = FakeApi()
        repeat(3) { api.streams += Channel<SubscribeEventsResponse>(Channel.UNLIMITED).also { it.trySend(SubscribeEventsResponse.newBuilder().setEnd(End.newBuilder().setReason("cursor invalid")).build()) } }
        val sink = Sink(this)
        conn({ api }, sink).start()
        advanceTimeBy(100); runCurrent()
        assertEquals(1, api.subscribes)
        assertEquals(listOf(1), sink.states.filterIsInstance<ConnState.Reconnecting>().map { it.attempt })
    }

    @Test fun `a different daemon behind the address drops the connection so a fresh snapshot is taken`() = runTest {
        val api = FakeApi()
        api.onHello = { n -> if (n >= 2) helloResponse(daemon = "other") else helloResponse() }
        val sink = Sink(this)
        conn({ api }, sink, heartbeat = 5_000).start()
        runCurrent()
        advanceTimeBy(5_100)
        assertTrue(sink.states.filterIsInstance<ConnState.Reconnecting>().any { "changed" in it.error })
    }

    @Test fun `a heartbeat that gets no answer drops the connection and it reconnects`() = runTest {
        val api = FakeApi()
        api.onHello = { n -> if (n == 2) CompletableDeferred<HelloResponse>().await() else helloResponse() }
        val sink = Sink(this)
        conn({ api }, sink, heartbeat = 5_000).start()
        runCurrent()
        advanceTimeBy(5_000 + 10_000 + 100)
        val dropped = sink.states.filterIsInstance<ConnState.Reconnecting>().first()
        assertTrue(dropped.error, "heartbeat" in dropped.error)
        advanceTimeBy(1_000)
        assertTrue(sink.last is ConnState.Ready)
    }

    @Test fun `a nudge on a ready connection checks it at once`() = runTest {
        val api = FakeApi()
        val sink = Sink(this)
        val c = conn({ api }, sink)
        c.start(); runCurrent()
        assertEquals(1, api.hellos)
        c.nudge(); runCurrent()
        assertEquals(2, api.hellos)
    }

    @Test fun `a host with another protocol, or a route that can't be used, stops with its reason`() = runTest {
        val sink = Sink(this)
        conn({ FakeApi().apply { onHello = { helloResponse(version = 2) } } }, sink).start()
        runCurrent()
        assertEquals(ConnState.Reason.Incompatible, (sink.last as ConnState.Stopped).reason)

        val sink2 = Sink(this)
        conn({ throw RouteException("relay routes arrive with pairing") }, sink2).start()
        runCurrent()
        assertEquals(ConnState.Stopped(ConnState.Reason.Config, "relay routes arrive with pairing"), sink2.last)
    }

    @Test fun `stopping ends the work`() = runTest {
        val api = FakeApi()
        val sink = Sink(this)
        val c = conn({ api }, sink)
        c.start(); runCurrent()
        c.stop()
        val hellos = api.hellos
        advanceTimeBy(60_000)
        assertEquals(hellos, api.hellos)
    }
}
