package dev.agentmesh.android

import dev.agentmesh.android.derive.Bucket
import dev.agentmesh.android.derive.BucketContext
import dev.agentmesh.android.derive.bucketOf
import dev.agentmesh.android.derive.groupByState
import dev.agentmesh.android.derive.statusSentence
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import org.junit.Assert.assertEquals
import org.junit.Test

/** The status buckets: the desktop's `buckets.test.ts`, case for case. */
class BucketsTest {
    private fun a(id: String, state: AgentState = AgentState.AGENT_STATE_IDLE, detail: String = "", exit: Int = 0, role: AgentRole = AgentRole.AGENT_ROLE_WORKER, peer: Boolean = false, run: String = "r1"): Agent =
        agent(id, runId = run, role = role, state = state, detail = detail).toBuilder().setExitCode(exit).setPeer(peer).build()

    private fun ctx(waiting: Set<String> = emptySet(), prs: Map<String, dev.agentmesh.gen.agentmesh.v1.AgentPullRequests> = emptyMap()) = BucketContext(waiting, prs)

    @Test fun `what needs the user comes first, a prompt, or a plan or question that waits`() {
        assertEquals(Bucket.Needs, bucketOf(a("a", AgentState.AGENT_STATE_NEEDS_INPUT), ctx()))
        assertEquals(Bucket.Needs, bucketOf(a("a"), ctx(waiting = setOf("a"))))
    }

    @Test fun `an error, a usage limit, a lost terminal and a nonzero exit are failed, a stop by the user or a clean exit is not`() {
        assertEquals(Bucket.Failed, bucketOf(a("a", AgentState.AGENT_STATE_ERROR), ctx()))
        assertEquals(Bucket.Failed, bucketOf(a("a", AgentState.AGENT_STATE_LIMITED), ctx()))
        assertEquals(Bucket.Failed, bucketOf(a("a", AgentState.AGENT_STATE_EXITED, "lost"), ctx()))
        assertEquals(Bucket.Failed, bucketOf(a("a", AgentState.AGENT_STATE_EXITED, exit = 2), ctx()))
        assertEquals(Bucket.Done, bucketOf(a("a", AgentState.AGENT_STATE_EXITED, "killed", exit = -1), ctx()))
        // What the host stores: a stop by a signal, and a lost terminal as exit code -1 with no detail.
        assertEquals(Bucket.Done, bucketOf(a("a", AgentState.AGENT_STATE_EXITED, "killed by signal", exit = -1), ctx()))
        assertEquals(Bucket.Failed, bucketOf(a("a", AgentState.AGENT_STATE_EXITED, "", exit = -1), ctx()))
        assertEquals(Bucket.Done, bucketOf(a("a", AgentState.AGENT_STATE_EXITED, "stopped", exit = -1), ctx()))
        assertEquals(Bucket.Failed, bucketOf(a("a", AgentState.AGENT_STATE_EXITED, "reopen failed: x", exit = -1), ctx()))
        assertEquals(Bucket.Done, bucketOf(a("a", AgentState.AGENT_STATE_EXITED, exit = 0), ctx()))
    }

    @Test fun `a start, a turn and a quiet agent are working`() {
        for (s in listOf(AgentState.AGENT_STATE_STARTING, AgentState.AGENT_STATE_WORKING, AgentState.AGENT_STATE_UNKNOWN)) assertEquals(Bucket.Working, bucketOf(a("a", s), ctx()))
    }

    @Test fun `a worker that stopped with an open PR is ready to review, and one whose PRs are all merged is done`() {
        fun open(vararg p: Pair<String, Boolean>) = mapOf("w" to agentPrs("w", *p.mapIndexed { i, (state, draft) -> pr("w", "u$i", i + 1, state = state, draft = draft) }.toTypedArray()))
        assertEquals(Bucket.Review, bucketOf(a("w"), ctx(prs = open("open" to false))))
        assertEquals(Bucket.Review, bucketOf(a("w", AgentState.AGENT_STATE_RETIRED), ctx(prs = open("open" to false))))
        assertEquals(Bucket.Done, bucketOf(a("w"), ctx(prs = open("merged" to false, "closed" to false))))
        // A draft isn't ready; a worker still working with an open PR is working.
        assertEquals(Bucket.Idle, bucketOf(a("w"), ctx(prs = open("open" to true))))
        assertEquals(Bucket.Working, bucketOf(a("w", AgentState.AGENT_STATE_WORKING), ctx(prs = open("open" to false))))
    }

    @Test fun `a lead, a reviewer and a standing peer are left out of review, and idle is told from done by the process`() {
        val open = mapOf("w" to agentPrs("w", pr("w", "u")))
        assertEquals(Bucket.Idle, bucketOf(a("w", role = AgentRole.AGENT_ROLE_ROOT), ctx(prs = open)))
        assertEquals(Bucket.Done, bucketOf(a("w", AgentState.AGENT_STATE_RETIRED, role = AgentRole.AGENT_ROLE_REVIEWER), ctx(prs = open)))
        assertEquals(Bucket.Idle, bucketOf(a("w", peer = true), ctx(prs = open)))
        assertEquals(Bucket.Done, bucketOf(a("w", AgentState.AGENT_STATE_RETIRED), ctx()))
    }

    @Test fun `grouped by state, the buckets in order with none empty and a closed run left out`() {
        val runs = mapOf("r1" to run("r1"), "r2" to run("r2", status = RunStatus.RUN_STATUS_CLOSED))
        val agents = listOf(a("a", AgentState.AGENT_STATE_WORKING), a("b", AgentState.AGENT_STATE_NEEDS_INPUT), a("c"), a("d", run = "r2")).associateBy { it.id }
        val g = groupByState(runs, agents, ctx())
        assertEquals(listOf("Needs you" to listOf("b"), "Working" to listOf("a"), "Idle" to listOf("c")), g.map { it.bucket.label to it.agents.map { x -> x.id } })
    }

    @Test fun `the sentence says a usage limit and an error, and why an exit failed`() {
        assertEquals("Hit a usage limit: 5h window; switch it to another model to go on", statusSentence(a("a", AgentState.AGENT_STATE_LIMITED, "5h window")))
        assertEquals("Its last turn ended in an error", statusSentence(a("a", AgentState.AGENT_STATE_ERROR)))
        assertEquals("Exited: reopen failed: x", statusSentence(a("a", AgentState.AGENT_STATE_EXITED, "reopen failed: x", exit = -1)))
        assertEquals("Lost: its terminal is gone", statusSentence(a("a", AgentState.AGENT_STATE_EXITED, "", exit = -1)))
        assertEquals("Exited with code 2", statusSentence(a("a", AgentState.AGENT_STATE_EXITED, "", exit = 2)))
        assertEquals("Stopped", statusSentence(a("a", AgentState.AGENT_STATE_EXITED, "killed by signal", exit = -1)))
    }
}
