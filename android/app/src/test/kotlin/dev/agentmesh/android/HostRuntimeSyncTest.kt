package dev.agentmesh.android

import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

/** What a host loads for a connection, loaded for every connection that syncs, however quickly a drop and a return follow. */
@OptIn(ExperimentalCoroutinesApi::class)
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36])
class HostRuntimeSyncTest {
    @get:Rule val tmp = TemporaryFolder()

    @Test fun `a reconnect that is over before anything looked at the state still loads the history again`() = runTest {
        val api = RecordingApi().apply {
            pages = mapOf("r1" to listOf(listOf(message("m1", from = "w1", to = "lead", type = dev.agentmesh.gen.agentmesh.v1.MessageType.MESSAGE_TYPE_INFO, at = 1_000))))
        }
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        // A test dispatcher: nothing runs between the calls below unless the test says so, as when a 0 ms reconnect is
        // over before the collector of a state flow gets to look.
        val rt = HostRuntime(host.id, repo, { _, _ -> api }, backgroundScope)
        rt.start(); runCurrent()
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1")), emptyList()))
        runCurrent()
        assertEquals(1, api.listCalls.count { it.first == "r1" })
        // The stream ends and comes back at once, with nothing run in between.
        rt.onState(ConnState.Reconnecting(0, 0, "end"))
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1")), emptyList()))
        runCurrent()
        assertEquals(2, api.listCalls.count { it.first == "r1" })
    }
}
