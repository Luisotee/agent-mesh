package dev.agentmesh.android

import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.store.applyFrame
import dev.agentmesh.android.store.mergeMessages
import dev.agentmesh.android.store.withPrompts
import dev.agentmesh.android.store.withQueued
import dev.agentmesh.android.store.withoutPrompt
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

    @Test fun `approval moves a worker to its execution model in the same conversation`() {
        val planning = a1.toBuilder().setModel("opus").setPhase(dev.agentmesh.gen.agentmesh.v1.AgentPhase.AGENT_PHASE_PLANNING).setDenyEdits(true).build()
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(planning)))
        s = applyFrame(s, reopenRequested(1, "a1"))
        assertTrue(s.agents["a1"]!!.hasPendingReopen())
        s = applyFrame(s, phaseChanged(2, "a1", dev.agentmesh.gen.agentmesh.v1.AgentPhase.AGENT_PHASE_EXECUTING))
        s = applyFrame(s, reopened(3, "a1", "sonnet", "t2"))
        val a = s.agents["a1"]!!
        assertEquals(dev.agentmesh.gen.agentmesh.v1.AgentPhase.AGENT_PHASE_EXECUTING, a.phase)
        assertEquals("sonnet", a.model); assertEquals("t2", a.terminalId); assertEquals(false, a.denyEdits)
        assertFalse(a.hasPendingReopen())
        assertEquals(planning.id, a.id)
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
            roleChanged(6, "a1", dev.agentmesh.gen.agentmesh.v1.AgentRole.AGENT_ROLE_WORKER),
        )
        val live = frames.fold(applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))) { s, f -> applyFrame(s, f) }
        // A later snapshot of the host's final state, as a fresh connection would get.
        val replay = applyFrame(HostState(), snapshot(live.runs.values.toList(), live.agents.values.toList(), seq = 6))
        assertEquals(live.runs, replay.runs)
        assertEquals(live.agents, replay.agents)
        assertEquals(live.cursor, replay.cursor)
        assertEquals(dev.agentmesh.gen.agentmesh.v1.AgentRole.AGENT_ROLE_WORKER, live.agents["a1"]!!.role)
    }

    @Test fun `loaded pages never overwrite a message an event updated`() {
        val m = message("m1", from = "a1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "from the event")
        val s = applyFrame(HostState(), messageSent(1, m))
        val merged = mergeMessages(s, listOf(m.toBuilder().setBody("from the page").build(), message("m2", from = "a1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO)))
        assertEquals("from the event", merged.messages["m1"]!!.body)
        assertEquals(2, merged.messages.size)
        assertFalse(merged.messages["m2"] == null)
    }

    @Test fun `a retired agent keeps its place but is stopped, and a stopped run closes`() {
        val r = run("r1")
        val waiting = a1.toBuilder().setState(AgentState.AGENT_STATE_NEEDS_INPUT).setNeedsInput(2).setStateDetail("trust?").build()
        var s = applyFrame(HostState(), snapshot(listOf(r), listOf(waiting)))
        s = applyFrame(s, retired(1, "a1"))
        val a = s.agents["a1"]!!
        assertEquals(AgentState.AGENT_STATE_RETIRED, a.state)
        assertEquals(0, a.needsInput)
        assertEquals("", a.stateDetail)
        // An unknown agent changes nothing but the cursor.
        assertEquals(setOf("a1"), applyFrame(s, retired(2, "ghost")).agents.keys)
        s = applyFrame(s, runStopped(3, "r1"))
        assertEquals(dev.agentmesh.gen.agentmesh.v1.RunStatus.RUN_STATUS_CLOSED, s.runs["r1"]!!.status)
        assertEquals(3L, s.cursor!!.seq)
        assertEquals("a run the app doesn't know changes nothing", s.runs.keys, applyFrame(s, runStopped(4, "r9")).runs.keys)
    }

    @Test fun `a deleted run takes its agents and messages with it, and no other run`() {
        val other = a1.toBuilder().setId("b1").setRunId("r2").build()
        var s = applyFrame(HostState(), snapshot(listOf(run("r1"), run("r2")), listOf(a1, other)))
        s = applyFrame(s, messageSent(1, message("m1", from = "a1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO)))
        assertEquals(setOf("r1", "r2"), s.runs.keys)
        s = applyFrame(s, runDeleted(2, "r1"))
        assertEquals(setOf("r2"), s.runs.keys)
        assertEquals(setOf("b1"), s.agents.keys)
        assertEquals(emptySet<String>(), s.messages.keys)
        assertEquals("a run the app doesn't know changes nothing", s.runs.keys, applyFrame(s, runDeleted(3, "r9")).runs.keys)
    }

    @Test fun `a run renamed while we listen takes its new title`() {
        var s = applyFrame(HostState(), snapshot(listOf(run("r1", "old")), emptyList()))
        s = applyFrame(s, runRenamed(1, "r1", "new title"))
        assertEquals("new title", s.runs["r1"]!!.title)
        assertEquals("a run the app doesn't know changes nothing", s.runs.keys, applyFrame(s, runRenamed(2, "r9", "x")).runs.keys)
    }

    @Test fun `a delivery that arrives before its message is applied when the page brings the message`() {
        val m = message("m1", from = "a1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO)
        var s = applyFrame(HostState(), delivered(1, "m1", at = 6_000))
        assertTrue(s.messages.isEmpty())
        s = mergeMessages(s, listOf(m))
        assertEquals(6, s.messages["m1"]!!.deliveredAt.seconds)
        assertTrue("used up", s.earlyDeliveries.isEmpty())
        // A snapshot forgets deliveries nobody asked for.
        assertTrue(applyFrame(applyFrame(HostState(), delivered(1, "zz")), snapshot(emptyList(), emptyList())).earlyDeliveries.isEmpty())
    }

    @Test fun `a run that starts while connected has nothing to load, its messages are complete as events bring them`() {
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))
        assertTrue(s.messagesLoaded.isEmpty())
        s = applyFrame(s, runStarted(1, run("r2")))
        assertEquals(setOf("r2"), s.messagesLoaded)
        // A run the snapshot already had is not made "loaded" by its start event arriving again.
        assertEquals(setOf("r2"), applyFrame(s, runStarted(2, run("r1"))).messagesLoaded)
    }

    @Test fun `too many early deliveries drop the oldest, not the newest`() {
        var s = HostState()
        for (i in 1..205) s = applyFrame(s, delivered(i.toLong(), "m$i", at = 6_000))
        assertEquals(200, s.earlyDeliveries.size)
        assertFalse("m1" in s.earlyDeliveries); assertTrue("m205" in s.earlyDeliveries)
    }

    @Test fun `an activity frame changes what a working agent runs, and only a working one`() {
        val working = agent("w1", state = AgentState.AGENT_STATE_WORKING, detail = "Bash")
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(working, a1), seq = 1))
        s = applyFrame(s, activity("w1", "Read", at = 8_000))
        assertEquals("Read", s.agents.getValue("w1").stateDetail)
        assertEquals(8_000L, s.agents.getValue("w1").lastActivity.seconds * 1000)
        // An idle agent's detail is its state's, not a tool; an unknown agent isn't there.
        assertEquals(s.agents.getValue("a1"), applyFrame(s, activity("a1", "Read")).agents.getValue("a1"))
        assertEquals(s, applyFrame(s, activity("nobody", "Read")))
        // The next state change says what it says: the frame was only a hint.
        s = applyFrame(s, stateChanged(2, "w1", AgentState.AGENT_STATE_WORKING, AgentState.AGENT_STATE_IDLE))
        assertEquals("", s.agents.getValue("w1").stateDetail)
    }

    @Test fun `a PR event replaces the PR of its url or adds it, and keeps the agent's others`() {
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1), seq = 1))
        s = applyFrame(s, prChanged(2, pr("a1", "u1", 1, checks = "pending")))
        s = applyFrame(s, prChanged(3, pr("a1", "u2", 2)))
        s = applyFrame(s, prChanged(4, pr("a1", "u1", 1, state = "merged", checks = "pass")))
        val prs = s.pullRequests.getValue("a1").pullRequestsList
        assertEquals(listOf("u1" to "merged", "u2" to "open"), prs.map { it.url to it.state })
    }

    @Test fun `a hold is a PR event too, so the hold shows at once`() {
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1), seq = 1))
        s = applyFrame(s, prChanged(2, pr("a1", "u1")))
        s = applyFrame(s, prChanged(3, pr("a1", "u1", held = true)))
        assertTrue(s.pullRequests.getValue("a1").getPullRequests(0).held)
    }

    @Test fun `a prompt joins the agent's queue in order, and leaves it once typed or taken back`() {
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))
        s = applyFrame(s, promptQueued(1, queuedPrompt("p2", "a1", at = 2_000)))
        s = applyFrame(s, promptQueued(2, queuedPrompt("p1", "a1", at = 1_000, held = "working")))
        assertEquals(listOf("p1", "p2"), s.prompts["a1"]!!.map { it.id })
        s = applyFrame(s, promptTyped(3, "a1", "p1"))
        assertEquals(listOf("p2"), s.prompts["a1"]!!.map { it.id })
        s = applyFrame(s, promptWithdrawn(4, "a1", "p2"))
        assertTrue(s.prompts["a1"]!!.isEmpty())
        // A requeue (its typing never reached the terminal) is queued again.
        s = applyFrame(s, promptQueued(5, queuedPrompt("p1", "a1")))
        assertEquals(listOf("p1"), s.prompts["a1"]!!.map { it.id })
        assertFalse("p1" in s.promptsDone)
    }

    @Test fun `a listing older than a change here, or than a typed prompt, never brings one back`() {
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))
        val asked = s.promptTouch["a1"] ?: 0
        s = applyFrame(s, promptQueued(1, queuedPrompt("p1", "a1")))
        // The queue changed after the listing was asked for: what we hold is newer than the answer.
        assertEquals(listOf("p1"), withPrompts(s, "a1", emptyList(), asked).prompts["a1"]!!.map { it.id })
        // A listing asked after the change is taken, minus a prompt the host already said was typed.
        s = applyFrame(s, promptTyped(2, "a1", "p0"))
        val now = s.promptTouch["a1"] ?: 0
        assertEquals(listOf("p1"), withPrompts(s, "a1", listOf(queuedPrompt("p0", "a1"), queuedPrompt("p1", "a1")), now).prompts["a1"]!!.map { it.id })
        // A prompt taken back here is gone for good, and an answer that queued one shows it before its event.
        s = withoutPrompt(s, "a1", "p1")
        assertTrue(s.prompts["a1"]!!.isEmpty())
        s = withQueued(s, queuedPrompt("p9", "a1"))
        assertEquals(listOf("p9"), s.prompts["a1"]!!.map { it.id })
        assertEquals(1, withQueued(s, queuedPrompt("p9", "a1")).prompts["a1"]!!.size)
    }

    /** The events and the host's listing of the same queue give the same state, so a screen that lists again after a snapshot agrees with one that followed the stream. */
    @Test fun `the queue kept by events is the one a listing gives`() {
        val frames = listOf(
            promptQueued(1, queuedPrompt("p1", "a1", at = 1_000)), promptQueued(2, queuedPrompt("p2", "a1", at = 2_000)),
            promptQueued(3, queuedPrompt("p3", "a1", at = 3_000)), promptTyped(4, "a1", "p1"), promptWithdrawn(5, "a1", "p3"),
        )
        val live = frames.fold(applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))) { s, f -> applyFrame(s, f) }
        val listed = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1), seq = 5)).let { withPrompts(it, "a1", listOf(queuedPrompt("p2", "a1", at = 2_000)), 0) }
        assertEquals(listed.prompts["a1"], live.prompts["a1"])
        // A snapshot drops the queues of agents the host no longer lists.
        assertTrue(applyFrame(live, snapshot(listOf(run("r1")), emptyList())).prompts.isEmpty())
    }

    @Test fun `a snapshot forgets which prompts were typed, so one the host requeued is listed again`() {
        var s = applyFrame(HostState(), snapshot(listOf(run("r1")), listOf(a1)))
        s = applyFrame(s, promptTyped(1, "a1", "p1"))
        val asked = s.promptTouch["a1"] ?: 0
        // The stream drops, the host requeues p1 (typing failed), a snapshot comes: a listing now brings it.
        s = applyFrame(s, snapshot(listOf(run("r1")), listOf(a1), seq = 9))
        assertTrue(s.promptsDone.isEmpty())
        // A listing asked before the snapshot is stale; one asked after it is taken.
        assertTrue(withPrompts(s, "a1", listOf(queuedPrompt("p1", "a1")), asked).prompts["a1"].isNullOrEmpty())
        assertEquals(listOf("p1"), withPrompts(s, "a1", listOf(queuedPrompt("p1", "a1")), s.promptTouch["a1"] ?: 0).prompts["a1"]!!.map { it.id })
    }
}
