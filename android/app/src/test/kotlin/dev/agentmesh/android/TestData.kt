package dev.agentmesh.android

import com.google.protobuf.Timestamp
import dev.agentmesh.gen.agentmesh.v1.Actor
import dev.agentmesh.gen.agentmesh.v1.ActorClass
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentExited
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentSpawned
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.AgentStateChanged
import dev.agentmesh.gen.agentmesh.v1.Cursor
import dev.agentmesh.gen.agentmesh.v1.Event
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.MessageSent
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.Run
import dev.agentmesh.gen.agentmesh.v1.RunStarted
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import dev.agentmesh.gen.agentmesh.v1.Snapshot
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse

fun ts(ms: Long): Timestamp = Timestamp.newBuilder().setSeconds(ms / 1000).setNanos(((ms % 1000) * 1_000_000).toInt()).build()

fun run(id: String, title: String = id, status: RunStatus = RunStatus.RUN_STATUS_OPEN, at: Long = 1_000): Run =
    Run.newBuilder().setId(id).setTitle(title).setStatus(status).setCreatedAt(ts(at)).build()

fun agent(
    id: String, runId: String = "r1", role: AgentRole = AgentRole.AGENT_ROLE_WORKER, parent: String = "",
    state: AgentState = AgentState.AGENT_STATE_IDLE, name: String = id, at: Long = 1_000, needsInput: Int = 0, detail: String = "",
): Agent = Agent.newBuilder().setId(id).setRunId(runId).setName(name).setRole(role).setParentId(parent).setState(state)
    .setNeedsInput(needsInput).setStateDetail(detail).setCreatedAt(ts(at)).setKind("claude").setRepo("/home/u/shop-api").build()

fun message(id: String, runId: String = "r1", from: String, to: String, type: MessageType, body: String = "", at: Long = 1_000): Message =
    Message.newBuilder().setId(id).setRunId(runId).setFrom(Actor.newBuilder().setClass_(ActorClass.ACTOR_CLASS_ROOT).setId(from)).setToId(to)
        .setType(type).setBody(body).setCreatedAt(ts(at)).build()

private const val GEN = "g1"

fun snapshot(runs: List<Run>, agents: List<Agent>, seq: Long = 0): SubscribeEventsResponse = SubscribeEventsResponse.newBuilder()
    .setSnapshot(Snapshot.newBuilder().addAllRuns(runs).addAllAgents(agents).setCursor(Cursor.newBuilder().setGeneration(GEN).setSeq(seq))).build()

private fun ev(seq: Long, at: Long = 5_000, agentId: String = "", runId: String = "", block: Event.Builder.() -> Unit): SubscribeEventsResponse =
    SubscribeEventsResponse.newBuilder().setEvent(Event.newBuilder().setSeq(seq).setGeneration(GEN).setTime(ts(at)).setAgentId(agentId).setRunId(runId).apply(block)).build()

fun runStarted(seq: Long, r: Run) = ev(seq, runId = r.id) { setRunStarted(RunStarted.newBuilder().setRun(r)) }
fun agentSpawned(seq: Long, a: Agent) = ev(seq, agentId = a.id, runId = a.runId) { setAgentSpawned(AgentSpawned.newBuilder().setAgent(a)) }
fun stateChanged(seq: Long, agentId: String, from: AgentState, to: AgentState, detail: String = "", needs: Int = 0, at: Long = 5_000) =
    ev(seq, at, agentId) { setAgentStateChanged(AgentStateChanged.newBuilder().setFrom(from).setTo(to).setDetail(detail).setNeedsInput(needs)) }
fun exited(seq: Long, agentId: String, code: Int, reason: String = "exited") =
    ev(seq, agentId = agentId) { setAgentExited(AgentExited.newBuilder().setExitCode(code).setReason(reason)) }
fun messageSent(seq: Long, m: Message, at: Long = 5_000) = ev(seq, at, runId = m.runId) { setMessageSent(MessageSent.newBuilder().setMessage(m)) }

fun phaseChanged(seq: Long, agentId: String, to: dev.agentmesh.gen.agentmesh.v1.AgentPhase) =
    ev(seq, agentId = agentId) { setAgentPhaseChanged(dev.agentmesh.gen.agentmesh.v1.AgentPhaseChanged.newBuilder().setTo(to).setWhy("plan approved")) }

fun roleChanged(seq: Long, agentId: String, to: dev.agentmesh.gen.agentmesh.v1.AgentRole) =
    ev(seq, agentId = agentId) { setAgentRoleChanged(dev.agentmesh.gen.agentmesh.v1.AgentRoleChanged.newBuilder().setTo(to).setWhy("change task")) }

fun reopened(seq: Long, agentId: String, model: String, terminal: String) =
    ev(seq, agentId = agentId) {
        setAgentReopened(
            dev.agentmesh.gen.agentmesh.v1.AgentReopened.newBuilder().setTerminalId(terminal).setSessionId("s1")
                .setReopen(dev.agentmesh.gen.agentmesh.v1.Reopen.newBuilder().setId("ro1").setModel(model).setEffort("high").setDenyEdits(false)),
        )
    }

fun reopenRequested(seq: Long, agentId: String) =
    ev(seq, agentId = agentId) { setAgentReopenRequested(dev.agentmesh.gen.agentmesh.v1.AgentReopenRequested.newBuilder().setReopen(dev.agentmesh.gen.agentmesh.v1.Reopen.newBuilder().setId("ro1"))) }

fun retired(seq: Long, agentId: String, runId: String = "r1", reason: String = "requested") =
    ev(seq, agentId = agentId, runId = runId) { setAgentRetired(dev.agentmesh.gen.agentmesh.v1.AgentRetired.newBuilder().setReason(reason)) }
fun runStopped(seq: Long, runId: String) =
    ev(seq, runId = runId) { setRunStopped(dev.agentmesh.gen.agentmesh.v1.RunStopped.getDefaultInstance()) }
fun runRenamed(seq: Long, runId: String, title: String) =
    ev(seq, runId = runId) { setRunRenamed(dev.agentmesh.gen.agentmesh.v1.RunRenamed.newBuilder().setTitle(title)) }
fun delivered(seq: Long, messageId: String, at: Long = 6_000) =
    ev(seq, at) { setMessageDelivered(dev.agentmesh.gen.agentmesh.v1.MessageDelivered.newBuilder().setMessageId(messageId)) }

fun activity(agentId: String, detail: String, at: Long = 7_000): SubscribeEventsResponse =
    SubscribeEventsResponse.newBuilder().setActivity(dev.agentmesh.gen.agentmesh.v1.AgentActivity.newBuilder().setRunId("r1").setAgentId(agentId).setDetail(detail).setTime(ts(at))).build()

fun pr(agentId: String, url: String, number: Int = 1, state: String = "open", draft: Boolean = false, checks: String = "pass", held: Boolean = false): dev.agentmesh.gen.agentmesh.v1.PullRequest =
    dev.agentmesh.gen.agentmesh.v1.PullRequest.newBuilder().setAgentId(agentId).setUrl(url).setNumber(number).setState(state).setDraft(draft).setChecks(checks).setHeld(held).build()

fun agentPrs(agentId: String, vararg prs: dev.agentmesh.gen.agentmesh.v1.PullRequest): dev.agentmesh.gen.agentmesh.v1.AgentPullRequests =
    dev.agentmesh.gen.agentmesh.v1.AgentPullRequests.newBuilder().setAgentId(agentId).setAgentName(agentId).addAllPullRequests(prs.toList()).build()

fun prChanged(seq: Long, p: dev.agentmesh.gen.agentmesh.v1.PullRequest) =
    ev(seq, agentId = p.agentId) { setPullRequestChanged(dev.agentmesh.gen.agentmesh.v1.PullRequestChanged.newBuilder().setPullRequest(p)) }

fun waiting(kind: dev.agentmesh.gen.agentmesh.v1.WaitingKind, agentId: String, runId: String = "r1", message: Message? = null, detail: String = ""): dev.agentmesh.gen.agentmesh.v1.Waiting =
    dev.agentmesh.gen.agentmesh.v1.Waiting.newBuilder().setKind(kind).setRunId(runId).setAgentId(agentId).setDetail(detail).apply { if (message != null) setMessage(message) }.build()

fun queuedPrompt(id: String, agentId: String, text: String = id, at: Long = 1_000, held: String = ""): dev.agentmesh.gen.agentmesh.v1.QueuedPrompt =
    dev.agentmesh.gen.agentmesh.v1.QueuedPrompt.newBuilder().setId(id).setAgentId(agentId).setText(text).setCreatedAt(ts(at)).setHeld(held).build()

fun promptQueued(seq: Long, q: dev.agentmesh.gen.agentmesh.v1.QueuedPrompt) =
    ev(seq, agentId = q.agentId) { setPromptQueued(dev.agentmesh.gen.agentmesh.v1.PromptQueued.newBuilder().setPrompt(q)) }
fun promptTyped(seq: Long, agentId: String, id: String) =
    ev(seq, agentId = agentId) { setPromptTyped(dev.agentmesh.gen.agentmesh.v1.PromptTyped.newBuilder().setPromptId(id)) }
fun promptWithdrawn(seq: Long, agentId: String, id: String) =
    ev(seq, agentId = agentId) { setPromptWithdrawn(dev.agentmesh.gen.agentmesh.v1.PromptWithdrawn.newBuilder().setPromptId(id)) }
