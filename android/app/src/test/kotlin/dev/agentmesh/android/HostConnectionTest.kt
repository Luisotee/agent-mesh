package dev.agentmesh.android

import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.ConnectionSink
import dev.agentmesh.android.net.HostConnection
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.PROTOCOL_VERSION
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

    override suspend fun listSettings(prefix: String): List<Setting> = emptyList()
    override suspend fun setSetting(key: String, value: com.google.protobuf.Value): Setting = error("unused")
    override suspend fun resetSetting(key: String): Setting = error("unused")
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
    private fun TestScope.conn(api: () -> MeshApi, sink: Sink, heartbeat: Long = 15_000) =
        HostConnection(backgroundScope, api, sink, now = { currentTime }, random = { 1.0 }, heartbeatMs = heartbeat, helloTimeoutMs = 10_000, baseMs = 500, maxMs = 15_000)

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
        advanceTimeBy(2_000)
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
