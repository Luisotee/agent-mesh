package dev.agentmesh.android

import com.google.protobuf.Value
import dev.agentmesh.android.net.ApiException
import com.google.protobuf.ByteString
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.TermEvent
import dev.agentmesh.android.net.TerminalSocket
import dev.agentmesh.gen.agentmesh.term.v1.AttachMode
import dev.agentmesh.gen.agentmesh.term.v1.Attached
import dev.agentmesh.gen.agentmesh.term.v1.Exited
import dev.agentmesh.gen.agentmesh.term.v1.Output
import dev.agentmesh.gen.agentmesh.term.v1.ReplayDone
import dev.agentmesh.gen.agentmesh.term.v1.Resized
import dev.agentmesh.gen.agentmesh.term.v1.Screen
import dev.agentmesh.gen.agentmesh.term.v1.TermFrame
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.channels.ReceiveChannel
import dev.agentmesh.gen.agentmesh.v1.Cursor
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.ListMessagesResponse
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.Setting
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.emptyFlow

/** A secret box that seals nothing: tests don't have the Keystore. */
object Plain : dev.agentmesh.android.hosts.SecretBox {
    override fun seal(plain: String) = plain
    override fun open(sealed: String) = sealed
}

/**
 * A copy of a synchronizedList taken under its own lock. Iterating one (count, any, a for loop) is not atomic, and fails
 * with a ConcurrentModificationException while another thread appends to it: read the fakes' recordings through this.
 */
fun <T> List<T>.snapshot(): List<T> = synchronized(this) { ArrayList(this) }

/**
 * A host that records what the app asks of it. Its handshake never answers (the screens are driven by the state a
 * test puts in), and its message pages are whatever the test gives it.
 */
class RecordingApi : MeshApi {
    val sent = mutableListOf<Triple<String, MessageType, String>>()
    val approved = mutableListOf<String>()
    val listCalls = mutableListOf<Pair<String, String>>()
    /** Pages by run: each call returns the next one, `more` set while pages remain. */
    var pages: Map<String, List<List<Message>>> = emptyMap()
    var failListings = 0
    /** Runs whose listing fails (the others work). */
    var failRuns: Set<String> = emptySet()
    /** When set, the next listing waits for it (a load in flight). */
    var listGate: CompletableDeferred<Unit>? = null
    var failSend: Exception? = null

    var revokedSelf = 0
    var failRevokeSelf: Exception? = null

    override suspend fun revokeSelf() {
        failRevokeSelf?.let { throw it }
        revokedSelf++
    }

    override suspend fun hello(): HelloResponse = CompletableDeferred<HelloResponse>().await()
    override fun subscribe(cursor: Cursor?): Flow<SubscribeEventsResponse> = emptyFlow()

    override suspend fun listMessages(runId: String, afterId: String): ListMessagesResponse {
        listCalls += runId to afterId
        listGate?.let { listGate = null; it.await() }
        if (runId in failRuns) throw ApiException(com.connectrpc.Code.UNAVAILABLE, "down")
        if (failListings > 0) { failListings--; throw ApiException(com.connectrpc.Code.UNAVAILABLE, "down") }
        val all = pages[runId].orEmpty()
        // The page after `afterId`: the one that follows the page whose last message that is.
        val idx = if (afterId.isEmpty()) 0 else all.indexOfFirst { p -> p.lastOrNull()?.id == afterId } + 1
        val page = all.getOrNull(idx).orEmpty()
        return ListMessagesResponse.newBuilder().addAllMessages(page).setMore(idx + 1 < all.size).build()
    }

    override suspend fun sendMessage(to: String, type: MessageType, body: String): Message {
        failSend?.let { throw it }
        sent += Triple(to, type, body)
        return message("sent-${sent.size}", from = "me", to = to, type = type, body = body, at = 90_000L + sent.size)
    }

    override suspend fun approveAgent(agent: String): Message {
        approved += agent
        return message("approved-${approved.size}", from = "me", to = agent, type = MessageType.MESSAGE_TYPE_ANSWER, body = "approved", at = 90_000L + approved.size)
    }

    /** Terminal streams the next attaches get, in order; each attach also asks for a ticket first. */
    val sockets = ArrayDeque<FakeSocket>()
    val tickets = mutableListOf<String>()
    var terminalError: Throwable? = null

    /** Whether each ticket asked for the input right. */
    val ticketInputs = mutableListOf<Boolean>()

    override suspend fun openTerminal(agent: String, input: Boolean): String {
        terminalError?.let { throw it }
        tickets += agent
        ticketInputs += input
        return "/v1/term/$agent?ticket=t${tickets.size}"
    }

    /** The host's shells; OpenShell adds one, CloseShell removes one. */
    @Volatile var shells: List<dev.agentmesh.gen.agentmesh.v1.Shell> = emptyList()
    val shellTickets = mutableListOf<String>()
    val shellTicketInputs = mutableListOf<Boolean>()
    val opened = mutableListOf<String>()
    val closed = mutableListOf<String>()
    var shellError: Throwable? = null

    override suspend fun openShellTerminal(shell: String, input: Boolean): String {
        terminalError?.let { throw it }
        shellTickets += shell
        shellTicketInputs += input
        return "/v1/shell/$shell?ticket=s${shellTickets.size}"
    }

    override suspend fun openShell(repo: String): dev.agentmesh.gen.agentmesh.v1.Shell {
        shellError?.let { throw it }
        opened += repo
        val s = dev.agentmesh.gen.agentmesh.v1.Shell.newBuilder().setId("sh${opened.size}").setRepo(repo).build()
        shells = shells + s
        return s
    }

    override suspend fun listShells(): List<dev.agentmesh.gen.agentmesh.v1.Shell> = shells

    override suspend fun closeShell(id: String) {
        shellError?.let { throw it }
        closed += id
        shells = shells.filter { it.id != id }
    }

    override fun connectTerminal(path: String): TerminalSocket = sockets.removeFirstOrNull() ?: FakeSocket()

    @Volatile var settings: List<Setting> = emptyList()
    override suspend fun listSettings(prefix: String): List<Setting> = settings
    override suspend fun setSetting(key: String, value: Value): Setting = error("unused")
    override suspend fun resetSetting(key: String): Setting = error("unused")

    /** What push calls reached the host: "register <endpoint>", "unregister", "test". */
    val push = java.util.Collections.synchronizedList(mutableListOf<String>())
    var failPush: Exception? = null
    /** The kinds each RegisterPush named, in order (an empty list: follow the host's). */
    val pushEvents = java.util.Collections.synchronizedList(mutableListOf<List<String>>())

    /** While set, RegisterPush hangs until it completes, even when cancelled: a request already on its way. */
    @Volatile var registerGate: kotlinx.coroutines.CompletableDeferred<Unit>? = null

    /** RegisterPush calls that began (a gated one hasn't finished). */
    val registerCalls = java.util.concurrent.atomic.AtomicInteger()

    override suspend fun registerPush(endpoint: String, p256dh: String, auth: String, events: List<String>): dev.agentmesh.gen.agentmesh.v1.PushRegistration {
        registerCalls.incrementAndGet()
        registerGate?.let { kotlinx.coroutines.withContext(kotlinx.coroutines.NonCancellable) { it.await() } }
        failPush?.let { throw it }
        push += "register $endpoint"
        pushEvents += events
        return dev.agentmesh.gen.agentmesh.v1.PushRegistration.getDefaultInstance()
    }

    /** While set, UnregisterPush hangs until it completes: a host that is slow to answer. */
    @Volatile var unregisterGate: kotlinx.coroutines.CompletableDeferred<Unit>? = null

    override suspend fun unregisterPush(): Boolean {
        unregisterGate?.await()
        push += "unregister"
        return true
    }

    override suspend fun testPush(): dev.agentmesh.gen.agentmesh.v1.PushRegistration {
        failPush?.let { throw it }
        push += "test"
        return dev.agentmesh.gen.agentmesh.v1.PushRegistration.getDefaultInstance()
    }

    // ── The chat, the model, the pull requests, the quota: scripted by the test, recorded for it to check ──

    val reads = java.util.Collections.synchronizedList(mutableListOf<Triple<String, Long, Boolean>>())
    @Volatile var transcript: (String, Long, Boolean) -> dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse = { _, _, _ -> dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse.getDefaultInstance() }
    @Volatile var failRead: Exception? = null

    override suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse {
        reads += Triple(agent, offset, backward)
        failRead?.let { throw it }
        return transcript(agent, offset, backward)
    }

    val prompts = java.util.Collections.synchronizedList(mutableListOf<Pair<String, String>>())
    @Volatile var failPrompt: Exception? = null

    /** What a prompt is answered with: set it to make the host queue the next one ("working", "draft" …), null types it at once. */
    @Volatile var queueNext: String? = null
    /** What ListPrompts answers, by agent; WithdrawPrompt takes from it. */
    val queued = java.util.concurrent.ConcurrentHashMap<String, List<dev.agentmesh.gen.agentmesh.v1.QueuedPrompt>>()
    val withdrawn = java.util.Collections.synchronizedList(mutableListOf<String>())
    val interrupts = java.util.Collections.synchronizedList(mutableListOf<String>())
    @Volatile var stopped = true
    @Volatile var failQueue: Exception? = null

    override suspend fun promptAgent(agent: String, text: String): dev.agentmesh.gen.agentmesh.v1.QueuedPrompt? {
        failPrompt?.let { throw it }
        prompts += agent to text
        val held = queueNext ?: return null
        queueNext = null
        val q = dev.agentmesh.gen.agentmesh.v1.QueuedPrompt.newBuilder().setId("q${prompts.size}").setAgentId(agent).setText(text).setHeld(held).build()
        queued[agent] = queued[agent].orEmpty() + q
        return q
    }

    override suspend fun listPrompts(agent: String): List<dev.agentmesh.gen.agentmesh.v1.QueuedPrompt> {
        failQueue?.let { throw it }
        return queued[agent].orEmpty()
    }

    override suspend fun withdrawPrompt(agent: String, promptId: String): dev.agentmesh.gen.agentmesh.v1.QueuedPrompt? {
        failQueue?.let { throw it }
        val list = queued[agent].orEmpty()
        val p = (if (promptId.isEmpty()) list.lastOrNull() else list.firstOrNull { it.id == promptId }) ?: return null
        queued[agent] = list - p
        withdrawn += p.id
        return p
    }

    /** What SendPromptAnyway reached the host with: agent, prompt, draft. */
    val overDrafts = java.util.Collections.synchronizedList(mutableListOf<List<String>>())
    @Volatile var failAnyway: Exception? = null

    override suspend fun sendPromptAnyway(agent: String, promptId: String, draft: String): String {
        failAnyway?.let { throw it }
        overDrafts += listOf(agent, promptId, draft)
        queued[agent] = queued[agent].orEmpty().filter { it.id != promptId }
        return draft
    }

    override suspend fun interruptAgent(agent: String): Boolean { interrupts += agent; return stopped }

    /** What SwitchAgent calls reached the host: agent, tier, model, effort. */
    val switches = java.util.Collections.synchronizedList(mutableListOf<List<String>>())
    @Volatile var failSwitch: Exception? = null

    override suspend fun switchAgent(agent: String, tier: String, model: String, effort: String): dev.agentmesh.gen.agentmesh.v1.SwitchAgentResponse {
        failSwitch?.let { throw it }
        switches += listOf(agent, tier, model, effort)
        return dev.agentmesh.gen.agentmesh.v1.SwitchAgentResponse.getDefaultInstance()
    }

    @Volatile var models: dev.agentmesh.gen.agentmesh.v1.CheckModelsResponse = dev.agentmesh.gen.agentmesh.v1.CheckModelsResponse.getDefaultInstance()
    var modelChecks = 0

    override suspend fun checkModels(): dev.agentmesh.gen.agentmesh.v1.CheckModelsResponse {
        modelChecks++
        return models
    }

    @Volatile var agentList: List<dev.agentmesh.gen.agentmesh.v1.Agent> = emptyList()
    var agentListings = 0
    @Volatile var failAgents: Exception? = null

    override suspend fun listAgents(): List<dev.agentmesh.gen.agentmesh.v1.Agent> {
        agentListings++
        failAgents?.let { throw it }
        return agentList
    }

    /** What `ListWaiting` answers; null is a host too old to have the action (the app derives what waits from the messages). */
    @Volatile var waitingList: List<dev.agentmesh.gen.agentmesh.v1.Waiting>? = null
    var waitingListings = 0
    @Volatile var failWaiting: Exception? = null

    override suspend fun listWaiting(): List<dev.agentmesh.gen.agentmesh.v1.Waiting> {
        waitingListings++
        failWaiting?.let { throw it }
        return waitingList ?: throw ApiException(com.connectrpc.Code.UNIMPLEMENTED, "unknown method")
    }

    /** Pull requests by run id. */
    @Volatile var pullRequests: Map<String, List<dev.agentmesh.gen.agentmesh.v1.AgentPullRequests>> = emptyMap()
    val prListings = java.util.Collections.synchronizedList(mutableListOf<String>())
    @Volatile var failPrs: Exception? = null

    override suspend fun listPullRequests(runId: String): List<dev.agentmesh.gen.agentmesh.v1.AgentPullRequests> {
        prListings += runId
        failPrs?.let { throw it }
        return pullRequests[runId].orEmpty()
    }

    val holds = java.util.Collections.synchronizedList(mutableListOf<Pair<String, Boolean>>())
    @Volatile var failHold: Exception? = null

    override suspend fun holdPullRequest(url: String, release: Boolean): List<dev.agentmesh.gen.agentmesh.v1.PullRequest> {
        failHold?.let { throw it }
        holds += url to release
        return listOf(dev.agentmesh.gen.agentmesh.v1.PullRequest.newBuilder().setUrl(url).setHeld(!release).build())
    }

    @Volatile var usage: dev.agentmesh.gen.agentmesh.v1.GetUsageResponse = dev.agentmesh.gen.agentmesh.v1.GetUsageResponse.getDefaultInstance()
    val usageCalls = java.util.Collections.synchronizedList(mutableListOf<Boolean>())

    override suspend fun getUsage(refresh: Boolean): dev.agentmesh.gen.agentmesh.v1.GetUsageResponse {
        usageCalls += refresh
        return usage
    }

    @Volatile var cost: dev.agentmesh.gen.agentmesh.v1.GetCostResponse = dev.agentmesh.gen.agentmesh.v1.GetCostResponse.getDefaultInstance()
    /** What GetCost asked: run id, since. */
    val costCalls = java.util.Collections.synchronizedList(mutableListOf<Pair<String, String>>())
    /** Whether each GetCost asked for the per-agent table. */
    val costAgents = java.util.Collections.synchronizedList(mutableListOf<Boolean>())
    @Volatile var failCost: Exception? = null

    override suspend fun getCost(runId: String, since: String, agents: Boolean): dev.agentmesh.gen.agentmesh.v1.GetCostResponse {
        costCalls += runId to since
        costAgents += agents
        failCost?.let { throw it }
        return cost
    }
    // ── Starting runs and spawning agents ──

    val runsStarted = java.util.Collections.synchronizedList(mutableListOf<String>())
    @Volatile var failStartRun: Exception? = null

    override suspend fun startRun(title: String): dev.agentmesh.gen.agentmesh.v1.Run {
        failStartRun?.let { throw it }
        runsStarted += title
        return run("run-${runsStarted.size}", title)
    }

    /** What each SpawnAgent asked: run, repo, slug, role, kind, tier, model, task. */
    val spawns = java.util.Collections.synchronizedList(mutableListOf<List<String>>())
    @Volatile var failSpawn: Exception? = null

    override suspend fun spawnAgent(runId: String, repo: String, slug: String, role: dev.agentmesh.gen.agentmesh.v1.AgentRole, kind: String, tier: String, model: String, task: String): dev.agentmesh.gen.agentmesh.v1.SpawnAgentResponse {
        failSpawn?.let { throw it }
        spawns += listOf(runId, repo, slug, role.name, kind, tier, model, task)
        return dev.agentmesh.gen.agentmesh.v1.SpawnAgentResponse.newBuilder().setAgent(agent("spawned-${spawns.size}", runId = runId)).build()
    }

    @Volatile var projects: List<dev.agentmesh.gen.agentmesh.v1.Project> = emptyList()
    var projectListings = 0
    @Volatile var failProjects: Exception? = null

    override suspend fun listProjects(): List<dev.agentmesh.gen.agentmesh.v1.Project> {
        projectListings++
        failProjects?.let { throw it }
        return projects
    }

    /** What each stop, retire, rename and run stop asked, in order ("stop a1", "retire a1", "rename r1 Title", "stop-run r1"). */
    val lifecycle = java.util.Collections.synchronizedList(mutableListOf<String>())
    @Volatile var failLifecycle: Exception? = null

    override suspend fun stopAgent(agent: String) { failLifecycle?.let { throw it }; lifecycle += "stop $agent" }
    override suspend fun retireAgent(agent: String) { failLifecycle?.let { throw it }; lifecycle += "retire $agent" }
    override suspend fun renameRun(runId: String, title: String) { failLifecycle?.let { throw it }; lifecycle += "rename $runId $title" }
    override suspend fun stopRun(runId: String): List<String> { failLifecycle?.let { throw it }; lifecycle += "stop-run $runId"; return emptyList() }
    override suspend fun deleteRun(runId: String): List<String> { failLifecycle?.let { throw it }; lifecycle += "delete-run $runId"; return emptyList() }
}

/** A terminal stream a test scripts: what the host sends is put in with [host], what the app sent is in [sent]. */
class FakeSocket : TerminalSocket {
    private val channel = Channel<TermEvent>(Channel.UNLIMITED)
    val sent = mutableListOf<TermFrame>()
    var closed = false
    override val events: ReceiveChannel<TermEvent> get() = channel
    override fun send(frame: TermFrame): Boolean { sent += frame; return true }
    override fun close() { closed = true; channel.close() }

    fun host(f: TermFrame) { channel.trySend(TermEvent.Frame(f)) }
    fun close(code: Int, reason: String = "") { channel.trySend(TermEvent.Closed(code, reason)); channel.close() }
    fun fail(e: Throwable) { channel.trySend(TermEvent.Failed(e)); channel.close() }
}

object Frames {
    fun attached(mode: AttachMode, epoch: Long = 1, offset: Long = 0, cols: Int = 40, rows: Int = 10, reanchored: Boolean = false) =
        TermFrame.newBuilder().setAttached(Attached.newBuilder().setMode(mode).setEpoch(epoch).setOffset(offset).setCols(cols).setRows(rows).setReanchored(reanchored)).build()

    fun screen(text: String, epoch: Long = 1, offset: Long = text.length.toLong()) =
        TermFrame.newBuilder().setScreen(Screen.newBuilder().setEpoch(epoch).setOffset(offset).setData(ByteString.copyFromUtf8(text))).build()

    fun output(text: String, offset: Long, epoch: Long = 1) =
        TermFrame.newBuilder().setOutput(Output.newBuilder().setEpoch(epoch).setOffset(offset).setData(ByteString.copyFromUtf8(text))).build()

    fun resized(epoch: Long, offset: Long, cols: Int, rows: Int) =
        TermFrame.newBuilder().setResized(Resized.newBuilder().setEpoch(epoch).setOffset(offset).setCols(cols).setRows(rows)).build()

    fun replayDone(offset: Long = 0) = TermFrame.newBuilder().setReplayDone(ReplayDone.newBuilder().setOffset(offset)).build()
    fun exited(code: Int = 0, signal: Int = 0) = TermFrame.newBuilder().setExited(Exited.newBuilder().setExitCode(code).setSignal(signal)).build()
}

/** A distributor a test drives: what the app registered, and whether one is installed and chosen. */
class FakePushChannel : dev.agentmesh.android.notify.PushChannel {
    var installed = listOf("io.heckel.ntfy")
    var chosen: String? = null
    val registered = java.util.Collections.synchronizedList(mutableListOf<String>())
    val unregistered = java.util.Collections.synchronizedList(mutableListOf<String>())
    override fun distributors() = installed
    override fun current() = chosen
    override fun pick(activity: android.app.Activity, done: (Boolean) -> Unit) {
        chosen = installed.firstOrNull()
        done(chosen != null)
    }
    override fun register(hostId: String, hostName: String) { registered += hostId }
    override fun unregister(hostId: String) { unregistered += hostId }
}

/** Endpoints kept in memory. */
class MemoryEndpoints : dev.agentmesh.android.notify.EndpointStore {
    val map = java.util.concurrent.ConcurrentHashMap<String, dev.agentmesh.android.notify.SavedEndpoint>()
    override fun get(hostId: String) = map[hostId]
    override fun put(hostId: String, e: dev.agentmesh.android.notify.SavedEndpoint) { map[hostId] = e }
    override fun remove(hostId: String) { map.remove(hostId) }

}
