package dev.agentmesh.android

import dev.agentmesh.android.derive.AgentRow
import dev.agentmesh.android.derive.RunRow
import dev.agentmesh.android.derive.WaitingKind
import dev.agentmesh.android.derive.buildTrees
import dev.agentmesh.android.derive.flatten
import dev.agentmesh.android.derive.repoName
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.android.derive.waitingOnYou
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class DeriveTest {
    private val lead = agent("lead", role = AgentRole.AGENT_ROLE_ROOT, at = 1_000)
    private val w1 = agent("w1", parent = "lead", at = 2_000)
    private val w2 = agent("w2", parent = "lead", at = 3_000)
    private val rev = agent("rev", role = AgentRole.AGENT_ROLE_REVIEWER, parent = "w1", at = 4_000)

    private fun byId(vararg a: dev.agentmesh.gen.agentmesh.v1.Agent) = a.associateBy { it.id }

    @Test fun `a run is a tree from its lead`() {
        val t = buildTrees(mapOf("r1" to run("r1")), byId(rev, w2, lead, w1)).single()
        assertEquals(listOf("lead"), t.roots.map { it.agent.id })
        assertEquals(listOf("w1", "w2"), t.roots[0].children.map { it.agent.id })
        assertEquals(listOf("rev"), t.roots[0].children[0].children.map { it.agent.id })
    }

    @Test fun `an agent whose parent is unknown is a root, and a parent loop terminates`() {
        val orphan = agent("orphan", parent = "nobody")
        assertEquals(setOf("lead", "orphan"), buildTrees(mapOf("r1" to run("r1")), byId(lead, orphan)).single().roots.map { it.agent.id }.toSet())
        val x = agent("x", parent = "y", at = 1)
        val y = agent("y", parent = "x", at = 2)
        val t = buildTrees(mapOf("r1" to run("r1")), byId(x, y)).single()
        assertEquals(1, t.roots.size)
        assertEquals(2, t.live)
    }

    @Test fun `runs needing the user come first, then open before closed, then the newest`() {
        val runs = mapOf(
            "old" to run("old", at = 1_000), "new" to run("new", at = 9_000), "done" to run("done", status = RunStatus.RUN_STATUS_CLOSED, at = 20_000),
            "ask" to run("ask", at = 500),
        )
        val agents = byId(agent("n", runId = "ask", state = AgentState.AGENT_STATE_NEEDS_INPUT))
        assertEquals(listOf("ask", "new", "old", "done"), buildTrees(runs, agents).map { it.run.id })
    }

    @Test fun `status sentences are plain`() {
        assertEquals("Working", statusSentence(agent("a", state = AgentState.AGENT_STATE_WORKING)))
        assertEquals("Needs you: Bash", statusSentence(agent("a", state = AgentState.AGENT_STATE_NEEDS_INPUT, needsInput = 1, detail = "Bash")))
        assertEquals("Needs you (2): Bash", statusSentence(agent("a", state = AgentState.AGENT_STATE_NEEDS_INPUT, needsInput = 2, detail = "Bash")))
        assertEquals("Finished", statusSentence(agent("a", state = AgentState.AGENT_STATE_EXITED)))
        assertEquals("Lost: its terminal is gone", statusSentence(agent("a", state = AgentState.AGENT_STATE_EXITED, detail = "lost")))
        assertEquals("Stopped", statusSentence(agent("a", state = AgentState.AGENT_STATE_EXITED, detail = "killed")))
    }

    @Test fun `repo names are the last folder`() {
        assertEquals("shop-api", repoName("/home/u/shop-api/"))
        assertEquals("shop-api", repoName("C:\\src\\shop-api"))
    }

    private val plan = message("m1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, at = 10_000)
    private val question = message("m2", from = "w2", to = "lead", type = MessageType.MESSAGE_TYPE_QUESTION, at = 11_000)

    @Test fun `a plan or question to the lead waits until someone answers its author`() {
        val runs = mapOf("r1" to run("r1"))
        val agents = byId(lead, w1, w2)
        val waiting = waitingOnYou(runs, agents, listOf(question, plan).associateBy { it.id })
        assertEquals(listOf(WaitingKind.Plan to "w1", WaitingKind.Question to "w2"), waiting.map { it.kind to it.agent.id })
        // The lead answered w1: that item closes. An answer before the plan doesn't count.
        val answer = message("m3", from = "lead", to = "w1", type = MessageType.MESSAGE_TYPE_ANSWER, at = 12_000)
        val early = message("m0", from = "lead", to = "w2", type = MessageType.MESSAGE_TYPE_ANSWER, at = 5_000)
        val after = waitingOnYou(runs, agents, listOf(question, plan, answer, early).associateBy { it.id })
        assertEquals(listOf("w2"), after.map { it.agent.id })
    }

    @Test fun `nothing waits on an author that has exited or in a closed run, and prompts come last`() {
        val gone = w1.toBuilder().setState(AgentState.AGENT_STATE_EXITED).build()
        val msgs = listOf(plan, question).associateBy { it.id }
        val asking = w2.toBuilder().setState(AgentState.AGENT_STATE_NEEDS_INPUT).build()
        val items = waitingOnYou(mapOf("r1" to run("r1")), byId(lead, gone, asking), msgs)
        assertEquals(listOf(WaitingKind.Question to "w2", WaitingKind.Input to "w2"), items.map { it.kind to it.agent.id })
        assertTrue(waitingOnYou(mapOf("r1" to run("r1", status = RunStatus.RUN_STATUS_CLOSED)), byId(lead, w1, w2), msgs).isEmpty())
    }

    @Test fun `flatten gives each agent its tree guides and hides collapsed runs`() {
        val trees = buildTrees(mapOf("r1" to run("r1"), "r2" to run("r2", at = 500)), byId(lead, w1, w2, rev) + byId(agent("solo", runId = "r2")))
        val rows = flatten(trees) { it.id == "r1" }
        val kinds = rows.map { if (it is RunRow) "run:${it.run.id}:${it.expanded}" else (it as AgentRow).agent.id }
        assertEquals(listOf("run:r1:true", "lead", "w1", "rev", "w2", "run:r2:false"), kinds)
        val w1Row = rows.filterIsInstance<AgentRow>().first { it.agent.id == "w1" }
        val revRow = rows.filterIsInstance<AgentRow>().first { it.agent.id == "rev" }
        val w2Row = rows.filterIsInstance<AgentRow>().first { it.agent.id == "w2" }
        assertEquals(1, w1Row.depth); assertEquals(false, w1Row.last)
        // rev hangs under w1, which has a later sibling: its row draws w1's vertical line.
        assertEquals(2, revRow.depth); assertEquals(listOf(true), revRow.guides); assertEquals(true, revRow.last)
        assertEquals(true, w2Row.last)
    }

    @Test fun `a retired agent's unanswered plan still waits on the user, since an answer wakes it`() {
        val retired = w1.toBuilder().setState(AgentState.AGENT_STATE_RETIRED).build()
        val waiting = waitingOnYou(mapOf("r1" to run("r1")), byId(lead, retired), listOf(plan).associateBy { it.id })
        assertEquals(listOf("w1"), waiting.map { it.agent.id })
        assertEquals("Retired: stopped on purpose, a message wakes it", statusSentence(retired))
    }

    @Test fun `a retired agent has an icon of its own`() {
        val st = AgentState.AGENT_STATE_RETIRED
        assertEquals(dev.agentmesh.android.ui.StateShape.Pause, dev.agentmesh.android.ui.shapeOf(st))
        for (other in listOf(AgentState.AGENT_STATE_STARTING, AgentState.AGENT_STATE_UNKNOWN, AgentState.AGENT_STATE_IDLE)) {
            assertTrue(dev.agentmesh.android.ui.shapeOf(other) != dev.agentmesh.android.ui.shapeOf(st))
        }
    }
}
