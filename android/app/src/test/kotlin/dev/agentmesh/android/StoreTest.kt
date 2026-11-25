package dev.agentmesh.android

import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.store.applyFrame
import dev.agentmesh.android.store.mergeMessages
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class StoreTest {
    private val a1 = agent("a1", state = AgentState.AGENT_STATE_IDLE)

    @Test fun `a snapshot replaces runs and agents and sets the cursor`() {
        val s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1), seq = 7))
        assertTrue(s.synced)
        assertEquals(setOf("r1"), s.runs.keys)
        assertEquals(setOf("a1"), s.agents.keys)
        assertEquals(7L, s.cursor!!.seq)
    }

    @Test fun `a snapshot drops the messages of runs the host no longer lists`() {
        val m = message("m1", runId = "gone", from = "a1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO)
        val before = HostState(messages = mapOf("m1" to m))
        assertTrue(applyFrame(before, snapshot(listOf(run("r1")), emptyList())).messages.isEmpty())
    }

    @Test fun `events change one thing each`() {
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))
        s = applyFrame(s, stateChanged(1, "a1", AgentState.AGENT_STATE_IDLE, AgentState.AGENT_STATE_NEEDS_INPUT, "Bash", needs = 1))
        assertEquals(AgentState.AGENT_STATE_NEEDS_INPUT, s.agents["a1"]!!.state)
        assertEquals("Bash", s.agents["a1"]!!.stateDetail)
        assertEquals(1, s.agents["a1"]!!.needsInput)
        s = applyFrame(s, exited(2, "a1", code = 3))
        assertEquals(AgentState.AGENT_STATE_EXITED, s.agents["a1"]!!.state)
        assertEquals(3, s.agents["a1"]!!.exitCode)
        assertEquals(0, s.agents["a1"]!!.needsInput)
        assertEquals(2L, s.cursor!!.seq)
        // An event about an agent we don't know moves the cursor and changes nothing else.
        assertEquals(s.agents, applyFrame(s, stateChanged(3, "ghost", AgentState.AGENT_STATE_IDLE, AgentState.AGENT_STATE_WORKING)).agents)
    }

    @Test fun `a resume keeps the state and marks it synced`() {
        val s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))
        val resumed = applyFrame(s.copy(synced = false), SubscribeEventsResponse.newBuilder().setResumed(dev.agentmesh.gen.agentmesh.v1.Resumed.getDefaultInstance()).build())
        assertTrue(resumed.synced)
        assertEquals(s.agents, resumed.agents)
    }

    /** The stream and its replay give the same state: a client that resumed after a drop ends where one that never dropped did. */
    @Test fun `a live stream and its replay from a snapshot give the same state`() {
        val a2 = agent("a2", parent = "a1", at = 2_000)
        val frames = listOf(
            runStarted(1, run("r2", at = 3_000)),
            agentSpawned(2, a2),
            stateChanged(3, "a2", AgentState.AGENT_STATE_STARTING, AgentState.AGENT_STATE_WORKING),
            stateChanged(4, "a1", AgentState.AGENT_STATE_IDLE, AgentState.AGENT_STATE_NEEDS_INPUT, "plan", 1),
            exited(5, "a2", 0),
        )
        val live = frames.fold(applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))) { s, f -> applyFrame(s, f) }
        // A later snapshot of the host's final state, as a fresh connection would get.
        val replay = applyFrame(HostState(), snapshot(live.runs.values.toList(), live.agents.values.toList(), seq = 5))
        assertEquals(live.runs, replay.runs)
        assertEquals(live.agents, replay.agents)
        assertEquals(live.cursor, replay.cursor)
    }

    @Test fun `loaded pages never overwrite a message an event updated`() {
        val m = message("m1", from = "a1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "from the event")
        val s = applyFrame(HostState(), messageSent(1, m))
        val merged = mergeMessages(s, listOf(m.toBuilder().setBody("from the page").build(), message("m2", from = "a1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO)))
        assertEquals("from the event", merged.messages["m1"]!!.body)
        assertEquals(2, merged.messages.size)
        assertFalse(merged.messages["m2"] == null)
    }
}
