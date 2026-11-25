package dev.agentmesh.android

import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.ConnectMeshApi
import dev.agentmesh.android.net.DefaultRouteConnector
import dev.agentmesh.android.net.Route
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.Dispatchers
import dev.agentmesh.android.term.TermClient
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.term.TermState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.async
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.withContext
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.flow.take
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class DaemonE2ETest {
    private var daemon: DaemonHarness? = null

    @Before fun setUp() {
        assumeTrue("no Go to build the daemon with, or SKIP_E2E is set", DaemonHarness.available)
        daemon = DaemonHarness.create().also { it.start() }
    }

    @After fun tearDown() { daemon?.close() }

    private fun api(token: String = daemon!!.token) =
        ConnectMeshApi(DefaultRouteConnector().connect(Route.Direct(daemon!!.url)) { token })

    @Test fun `hello over loopback http answers as a device`() = runBlocking {
        val hello = api().hello()
        assertEquals(daemon!!.daemonId, hello.daemonId)
        assertEquals(dev.agentmesh.gen.agentmesh.v1.ActorClass.ACTOR_CLASS_DEVICE, hello.actor.class_)
        assertTrue(hello.hasServerTime())
    }

    @Test fun `a wrong token is refused as unauthenticated`() = runBlocking {
        val e = runCatching { api("not-a-token").hello() }.exceptionOrNull()
        assertTrue("got $e", e is ApiException && e.refusedToken)
    }

    @Test fun `the event stream starts with a snapshot and then carries live events`() = runBlocking {
        val d = daemon!!
        val frames = withTimeout(30_000) {
            coroutineScope {
                // A run started once the snapshot arrived comes as an event on the same open stream.
                val snapshot = CompletableDeferred<Unit>()
                val got = async(Dispatchers.Default) {
                    api().subscribe(null).onEach { snapshot.complete(Unit) }.take(2).toList()
                }
                snapshot.await()
                withContext(Dispatchers.IO) { d.ctl("start-run", "--title", "from the phone test") }
                got.await()
            }
        }
        assertEquals(SubscribeEventsResponse.FrameCase.SNAPSHOT, frames.first().frameCase)
        assertEquals(SubscribeEventsResponse.FrameCase.EVENT, frames.last().frameCase)
        assertTrue(frames.last().event.hasRunStarted())
    }

    @Test fun `a read-only terminal attaches to a real agent, shows its screen and follows its output`() = runBlocking {
        val d = daemon!!
        val repo = d.repo("shop-api")
        val run = d.ctl("start-run", "--title", "term").getValue("run").jsonObject.getValue("id").jsonPrimitive.content
        val agent = d.ctl("spawn", "--run-id", run, "--repo", repo.path, "--slug", "fix", "--kind", "claude", "--role", "worker", "--model", "haiku")
            .getValue("agent").jsonObject.getValue("id").jsonPrimitive.content
        // The fake claude is up once its SessionStart hook made it idle.
        withTimeout(30_000) {
            while (d.ctl("agent", "--agent", agent).getValue("agent").jsonObject.getValue("state").jsonPrimitive.content != "AGENT_STATE_IDLE") kotlinx.coroutines.delay(100)
        }
        val dispatcher = Dispatchers.Default.limitedParallelism(1)
        val screen = TermScreen()
        val client = TermClient(CoroutineScope(dispatcher), agent, { api() }, screen)
        client.start()
        suspend fun screenText() = withContext(dispatcher) { screen.visibleText() }
        try {
            withTimeout(20_000) { while (client.state.value != TermState.Live || "fake claude" !in screenText()) kotlinx.coroutines.delay(50) }
            assertTrue(screenText().lines().first().startsWith("fake claude"))
            // The agent's output arrives live: the doorbell for a task makes the fake print what it did.
            withContext(Dispatchers.IO) { d.ctl("send", "--to", agent, "--type", "task", "--body", "fix the typo") }
            withTimeout(20_000) { while ("done:" !in screenText()) kotlinx.coroutines.delay(50) }
            assertEquals(TermState.Live, client.state.value)
        } finally {
            client.stop()
        }
    }
}
