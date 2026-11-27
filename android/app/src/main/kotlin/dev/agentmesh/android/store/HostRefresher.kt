package dev.agentmesh.android.store

import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentPullRequests
import dev.agentmesh.gen.agentmesh.v1.Event
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/**
 * Three things the event stream doesn't carry, read from the host again when they may have changed; the desktop's
 * `refresh.ts`, with the same rules.
 *
 * What waits on the user is the host's to say (`ListWaiting`), so it is listed again, after a short pause that folds a
 * burst of events into one call, whenever an event could change it: a plan, question or answer, an agent starting,
 * changing state, exiting or being retired, a run stopping. A list that was on its way while one of those arrived may be
 * stale: it is shown, and fetched again after the pause.
 *
 * The pull requests (`ListPullRequests`, which the host caches for two minutes) are read at connect and every
 * [prsEveryMs]; a PR that changes between is told by an event [applyFrame] applies itself.
 *
 * An agent that went quiet shows as `unknown` only when the host is asked (the state is derived from the time and the
 * terminal's output when read, never stored or sent as an event), so the agents are listed again every [agentsEveryMs];
 * an agent an event touched meanwhile keeps what the event said.
 */
class HostRefresher(
    private val scope: CoroutineScope,
    private val api: () -> MeshApi?,
    private val state: () -> HostState,
    private val update: ((HostState) -> HostState) -> Unit,
    private val log: (String) -> Unit,
    private val debounceMs: Long = 200,
    private val agentsEveryMs: Long = 30_000,
    private val prsEveryMs: Long = 60_000,
) {
    @Volatile private var stopped = false
    @Volatile private var waitingTouches = 0
    @Volatile private var agentTouches = 0
    @Volatile private var prTouches = 0
    private var pause: Job? = null
    private var waitingLoading = false
    private var prsLoading = false
    private var waitingFailed = false
    private var complained = false
    private var prsComplained = false
    private var tickers: List<Job> = emptyList()

    private fun complain(what: String, e: Throwable) {
        // A host that doesn't know the action (an older daemon) would say so every 30 s: once is enough.
        if (complained) return
        complained = true
        log("couldn't $what: ${e.message ?: e.javaClass.simpleName}")
    }

    private val ready get() = state().let { it.conn is ConnState.Ready && it.synced }

    private suspend fun loadWaiting() {
        if (waitingLoading) {
            // One call at a time; what changed meanwhile is asked for after the pause.
            schedule()
            return
        }
        val api = api() ?: return
        waitingLoading = true
        try {
            val seen = waitingTouches
            val list = api.listWaiting()
            if (stopped) return
            waitingFailed = false
            complained = false
            // Shown even if an event overtook it (a busy host answers slower than events come); the event scheduled a call,
            // or this one does, to correct it.
            update { withWaiting(it, list) }
            if (seen != waitingTouches) schedule()
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            // The list stays as it was; the next tick asks again.
            waitingFailed = true
            complain("list what waits on you", e)
        } finally {
            waitingLoading = false
        }
    }

    /** At most one call per [debounceMs], however many events come: a burst is one call, and a steady stream can't starve it. */
    private fun schedule() {
        if (stopped || pause?.isActive == true) return
        pause = scope.launch {
            delay(debounceMs)
            pause = null
            loadWaiting()
        }
    }

    private suspend fun loadPullRequests() {
        if (prsLoading || !ready) return
        val api = api() ?: return
        // The host answers per run (a device names it): one call for each run still open, side by side.
        val runIds = state().runs.values.filter { it.status != RunStatus.RUN_STATUS_CLOSED }.map { it.id }
        prsLoading = true
        val seen = prTouches
        var again = false
        try {
            val results = coroutineScope { runIds.map { id -> async { runCatching { api.listPullRequests(id) } } }.awaitAll() }
            if (stopped) return
            results.firstOrNull { it.exceptionOrNull() is CancellationException }?.let { throw it.exceptionOrNull()!! }
            if (seen != prTouches) {
                // An event (a merge, a hold) came while the call was on its way: its answer may predate it. Keep what the event said, and ask again.
                again = true
                return
            }
            val got: List<AgentPullRequests> = results.flatMap { it.getOrNull().orEmpty() }
            val failed = results.firstNotNullOfOrNull { it.exceptionOrNull() }
            // A run that failed keeps what was known of its agents; the others are replaced.
            val failedRuns = runIds.filterIndexed { i, _ -> results[i].isFailure }.toSet()
            update { s ->
                val kept = s.pullRequests.values.filter { s.agents[it.agentId]?.runId in failedRuns }
                withPullRequests(s, got + kept)
            }
            if (failed != null) {
                // Said once (a host without `gh` or an older daemon would say it every minute); the buckets then lack "ready to review".
                if (!prsComplained) log("couldn't read the pull requests: ${failed.message ?: failed.javaClass.simpleName}")
                prsComplained = true
            } else prsComplained = false
        } finally {
            prsLoading = false
            if (again) scope.launch { loadPullRequests() }
        }
    }

    private suspend fun loadAgents() {
        if (!ready) return
        val api = api() ?: return
        if (waitingFailed) {
            // One call per tick, so no pause is needed.
            waitingFailed = false
            waitingTouches++
            scope.launch { loadWaiting() }
        }
        val seen = agentTouches
        try {
            val list = api.listAgents()
            if (stopped || seen != agentTouches) return
            var changed = false
            update { s ->
                val next = s.agents.toMutableMap()
                for (a: Agent in list) if (next[a.id] != a) { next[a.id] = a; changed = true }
                if (changed) s.copy(agents = next) else s
            }
            if (changed) {
                // A prompt aged into unknown, or the other way round, changes what waits.
                waitingTouches++
                schedule()
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            complain("refresh the agents", e)
        }
    }

    /** Call with every event of the stream, after it was applied to the state. */
    fun event(e: Event) {
        when (e.payloadCase) {
            Event.PayloadCase.MESSAGE_SENT -> {
                val t = e.messageSent.message.type
                if (t == MessageType.MESSAGE_TYPE_PLAN || t == MessageType.MESSAGE_TYPE_QUESTION || t == MessageType.MESSAGE_TYPE_ANSWER) {
                    waitingTouches++
                    schedule()
                }
            }
            Event.PayloadCase.AGENT_SPAWNED, Event.PayloadCase.AGENT_STATE_CHANGED, Event.PayloadCase.AGENT_EXITED, Event.PayloadCase.AGENT_RETIRED -> {
                agentTouches++
                waitingTouches++
                schedule()
            }
            // These change an agent the state holds too, so a list of the agents that was on its way is older than them.
            Event.PayloadCase.AGENT_REOPEN_REQUESTED, Event.PayloadCase.AGENT_REOPEN_DROPPED, Event.PayloadCase.AGENT_REOPENED,
            Event.PayloadCase.AGENT_PHASE_CHANGED, Event.PayloadCase.AGENT_ROLE_CHANGED, Event.PayloadCase.AGENT_ADOPTED -> agentTouches++
            Event.PayloadCase.PULL_REQUEST_CHANGED -> prTouches++
            Event.PayloadCase.RUN_STOPPED -> {
                waitingTouches++
                schedule()
            }
            else -> {}
        }
    }

    /** A snapshot arrived (the first connection, or a resync): list what waits, and the pull requests, at once. */
    fun synced() {
        waitingTouches++
        pause?.cancel()
        pause = null
        scope.launch { loadWaiting() }
        scope.launch { loadPullRequests() }
    }

    fun start() {
        if (tickers.isNotEmpty()) return
        stopped = false
        tickers = listOf(
            scope.launch { while (isActive) { delay(agentsEveryMs); loadAgents() } },
            scope.launch { while (isActive) { delay(prsEveryMs); loadPullRequests() } },
        )
    }

    fun stop() {
        stopped = true
        pause?.cancel()
        tickers.forEach { it.cancel() }
        tickers = emptyList()
    }
}
