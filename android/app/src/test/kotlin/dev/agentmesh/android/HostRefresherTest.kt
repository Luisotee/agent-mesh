package dev.agentmesh.android

import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.store.HostRefresher
import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.store.applyFrame
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.WaitingKind
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** What the stream doesn't carry, read again when it may have changed: the desktop's `refresh.ts`, with its rules. */
@OptIn(ExperimentalCoroutinesApi::class)
class HostRefresherTest {
    private class Rig(val api: dev.agentmesh.android.net.MeshApi, scope: kotlinx.coroutines.CoroutineScope, runs: List<dev.agentmesh.gen.agentmesh.v1.Run> = listOf(run("r1")), agents: List<dev.agentmesh.gen.agentmesh.v1.Agent> = emptyList()) {
        var state = applyFrame(HostState(conn = ConnState.Ready(HelloResponse.getDefaultInstance())), snapshot(runs, agents, seq = 1))
        val logs = mutableListOf<String>()
        val refresher = HostRefresher(scope, { api }, { state }, { f -> state = f(state) }, { logs += it }, debounceMs = 200, agentsEveryMs = 30_000, prsEveryMs = 60_000)
    }

    @Test fun `a synced connection lists what waits and the pull requests at once`() = runTest {
        val api = RecordingApi().apply {
            waitingList = listOf(waiting(WaitingKind.WAITING_KIND_PLAN, "w1"))
            pullRequests = mapOf("r1" to listOf(agentPrs("w1", pr("w1", "u1"))))
        }
        val rig = Rig(api, backgroundScope, agents = listOf(agent("w1")))
        rig.refresher.synced(); runCurrent()
        assertEquals(1, rig.state.waiting!!.size)
        assertEquals(setOf("w1"), rig.state.pullRequests.keys)
        assertEquals(listOf("r1"), api.prListings.toList())
    }

    @Test fun `a closed run's pull requests are not asked for`() = runTest {
        val api = RecordingApi()
        val rig = Rig(api, backgroundScope, runs = listOf(run("r1"), run("r2", status = dev.agentmesh.gen.agentmesh.v1.RunStatus.RUN_STATUS_CLOSED)))
        rig.refresher.synced(); runCurrent()
        assertEquals(listOf("r1"), api.prListings.toList())
    }

    @Test fun `a burst of events is one call after the pause`() = runTest {
        val api = RecordingApi()
        val rig = Rig(api, backgroundScope, agents = listOf(agent("w1")))
        rig.refresher.synced(); runCurrent()
        val base = api.waitingListings
        for (i in 1..5) rig.refresher.event(stateChanged(i + 1L, "w1", AgentState.AGENT_STATE_IDLE, AgentState.AGENT_STATE_WORKING).event)
        rig.refresher.event(messageSent(9, message("m1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN)).event)
        advanceTimeBy(150); runCurrent()
        assertEquals(base, api.waitingListings)
        advanceTimeBy(100); runCurrent()
        assertEquals(base + 1, api.waitingListings)
    }

    @Test fun `an info message changes nothing that waits`() = runTest {
        val api = RecordingApi()
        val rig = Rig(api, backgroundScope)
        rig.refresher.synced(); runCurrent()
        val base = api.waitingListings
        rig.refresher.event(messageSent(2, message("m1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO)).event)
        advanceTimeBy(1_000); runCurrent()
        assertEquals(base, api.waitingListings)
    }

    @Test fun `the pull requests are read again every minute, and a failed run keeps what was known of its agents`() = runTest {
        val api = RecordingApi().apply { pullRequests = mapOf("r1" to listOf(agentPrs("w1", pr("w1", "u1")))) }
        val rig = Rig(api, backgroundScope, agents = listOf(agent("w1")))
        rig.refresher.start(); rig.refresher.synced(); runCurrent()
        assertEquals(1, api.prListings.size)
        api.failPrs = dev.agentmesh.android.net.ApiException(com.connectrpc.Code.UNAVAILABLE, "gh missing")
        advanceTimeBy(60_100); runCurrent()
        assertEquals(2, api.prListings.size)
        assertEquals(setOf("w1"), rig.state.pullRequests.keys)
        assertEquals(1, rig.logs.size)
        advanceTimeBy(60_000); runCurrent()
        // Said once, not every minute.
        assertEquals(1, rig.logs.size)
        rig.refresher.stop()
    }

    @Test fun `a host that doesn't know the action leaves waiting unknown, and says so once`() = runTest {
        val api = RecordingApi().apply { failWaiting = dev.agentmesh.android.net.ApiException(com.connectrpc.Code.UNIMPLEMENTED, "unknown method") }
        val rig = Rig(api, backgroundScope)
        rig.refresher.synced(); runCurrent()
        assertNull(rig.state.waiting)
        assertEquals(1, rig.logs.size)
        rig.refresher.event(stateChanged(2, "w1", AgentState.AGENT_STATE_IDLE, AgentState.AGENT_STATE_WORKING).event)
        advanceTimeBy(300); runCurrent()
        assertEquals(1, rig.logs.size)
    }

    @Test fun `the agents are listed again every 30 s, a quiet one shows as unknown, and an event in between wins`() = runTest {
        val api = RecordingApi()
        val rig = Rig(api, backgroundScope, agents = listOf(agent("w1", state = AgentState.AGENT_STATE_WORKING)))
        rig.refresher.start(); runCurrent()
        api.agentList = listOf(agent("w1", state = AgentState.AGENT_STATE_UNKNOWN))
        advanceTimeBy(30_100); runCurrent()
        assertEquals(AgentState.AGENT_STATE_UNKNOWN, rig.state.agents.getValue("w1").state)
        // A listing that was on its way when an event came is older than it: dropped.
        lateinit var rig2: Rig
        val slow = object : dev.agentmesh.android.net.MeshApi by api {
            override suspend fun listAgents(): List<dev.agentmesh.gen.agentmesh.v1.Agent> {
                rig2.refresher.event(stateChanged(5, "w1", AgentState.AGENT_STATE_UNKNOWN, AgentState.AGENT_STATE_IDLE).event)
                return api.listAgents()
            }
        }
        rig2 = Rig(slow, backgroundScope, agents = listOf(agent("w1", state = AgentState.AGENT_STATE_WORKING)))
        rig2.refresher.start(); runCurrent()
        api.agentList = listOf(agent("w1", state = AgentState.AGENT_STATE_UNKNOWN))
        advanceTimeBy(30_100); runCurrent()
        assertEquals(AgentState.AGENT_STATE_WORKING, rig2.state.agents.getValue("w1").state)
        rig.refresher.stop(); rig2.refresher.stop()
    }

    @Test fun `nothing is asked while the connection isn't ready`() = runTest {
        val api = RecordingApi()
        val rig = Rig(api, backgroundScope, agents = listOf(agent("w1")))
        rig.state = rig.state.copy(conn = ConnState.Connecting, synced = false)
        rig.refresher.start(); runCurrent()
        advanceTimeBy(125_000); runCurrent()
        assertTrue(api.prListings.isEmpty())
        assertEquals(0, api.agentListings)
        rig.refresher.stop()
    }
}
