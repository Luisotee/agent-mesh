package dev.agentmesh.android.derive

import dev.agentmesh.android.store.createdMillis
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentPullRequests
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.Run
import dev.agentmesh.gen.agentmesh.v1.RunStatus

/**
 * The status buckets of `docs/apps.md` ("Status buckets per workspace"): what a daily user triages by. An agent is in
 * exactly one, the first that fits. The desktop's `buckets.ts`, with its rules.
 */
enum class Bucket(val label: String) {
    Needs("Needs you"),
    Failed("Failed"),
    Review("Ready to review"),
    Working("Working"),
    Idle("Idle"),
    Done("Done"),
}

/** What a bucket is decided from besides the agent: who waits on the user (`ListWaiting`) and the PRs (`ListPullRequests`). */
class BucketContext(val waiting: Set<String> = emptySet(), val pullRequests: Map<String, AgentPullRequests> = emptyMap())

private fun isOpen(p: dev.agentmesh.gen.agentmesh.v1.PullRequest) = p.state == "open" && !p.draft

fun bucketOf(a: Agent, ctx: BucketContext): Bucket {
    if (a.state == AgentState.AGENT_STATE_NEEDS_INPUT || a.id in ctx.waiting) return Bucket.Needs
    val failedExit = a.state == AgentState.AGENT_STATE_EXITED && exitKind(a) == ExitKind.Failed
    if (a.state == AgentState.AGENT_STATE_ERROR || a.state == AgentState.AGENT_STATE_LIMITED || failedExit) return Bucket.Failed
    if (a.state == AgentState.AGENT_STATE_WORKING || a.state == AgentState.AGENT_STATE_STARTING || a.state == AgentState.AGENT_STATE_UNKNOWN) return Bucket.Working
    // Not working and nothing wrong: a change worker with an open PR waits for the user's review; one whose PRs are
    // all merged or closed is done; the rest is told by whether its process is still there.
    val prs = ctx.pullRequests[a.id]?.pullRequestsList.orEmpty()
    if (a.role == AgentRole.AGENT_ROLE_WORKER && !a.peer) {
        if (prs.any(::isOpen)) return Bucket.Review
        if (prs.isNotEmpty() && prs.all { it.state != "open" }) return Bucket.Done
    }
    return if (a.state == AgentState.AGENT_STATE_IDLE) Bucket.Idle else Bucket.Done
}

class BucketGroup(val bucket: Bucket, val agents: List<Agent>)

/**
 * The agents of the open runs by bucket, in the order of [Bucket] (what asks for the user first); an empty bucket is left
 * out, and a closed run's agents are history, in the run view. Newest first within a bucket.
 */
fun groupByState(runs: Map<String, Run>, agents: Map<String, Agent>, ctx: BucketContext): List<BucketGroup> {
    val by = HashMap<Bucket, MutableList<Agent>>()
    for (a in agents.values) {
        if (runs[a.runId]?.status == RunStatus.RUN_STATUS_CLOSED) continue
        by.getOrPut(bucketOf(a, ctx)) { ArrayList() }.add(a)
    }
    return Bucket.entries.filter { it in by }.map { b ->
        BucketGroup(b, by.getValue(b).sortedWith(compareByDescending<Agent> { it.createdMillis }.thenBy { it.name }))
    }
}
