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
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger

private const val RETRY_PRS_MS = 1_000L

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
 *
 * Unlike the desktop's single thread, events, the connection and the tickers come on several: the counters and flags are
 * atomic or kept under [lock], and everything it launches belongs to one job per lifetime, cancelled by [stop], so a
 * removed host asks for nothing more. A connection that syncs again ([synced]) or a daemon that was replaced ([reset])
 * starts a new epoch: an answer that was on its way from an earlier one is dropped, and doesn't hold up the new asks.
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
    private val lock = Any()
    private var life: Job = SupervisorJob(scope.coroutineContext[Job])
    @Volatile private var stopped = false
    private val epoch = AtomicInteger(0)
    private val waitingTouches = AtomicInteger(0)
    private val agentTouches = AtomicInteger(0)
    private val prTouches = AtomicInteger(0)
    private var pause: Job? = null
    // The epoch of the call that is out, or -1: a call of an older epoch doesn't count, and its answer is dropped.
    private var waitingLoading = -1
    private var prsLoading = -1
    private val waitingFailed = AtomicBoolean(false)
    private val waitingComplained = AtomicBoolean(false)
    private val agentsComplained = AtomicBoolean(false)
    private val prsComplained = AtomicBoolean(false)
    private var tickers: List<Job> = emptyList()

    private fun launch(block: suspend CoroutineScope.() -> Unit): Job? = synchronized(lock) {
        if (stopped) null else CoroutineScope(scope.coroutineContext + life).launch { block() }
    }

    // A host that doesn't know an action (an older daemon) would say so every 30 s: once is enough, for each call apart.
    private fun complain(flag: AtomicBoolean, what: String, e: Throwable) {
        if (!flag.compareAndSet(false, true)) return
        log("couldn't $what: ${e.message ?: e.javaClass.simpleName}")
    }

    private val ready get() = state().let { it.conn is ConnState.Ready && it.synced }

    private suspend fun loadWaiting() {
        if (stopped) return
        val e0 = epoch.get()
        synchronized(lock) {
            if (waitingLoading == e0) {
                // One call at a time; what changed meanwhile is asked for after the pause.
                schedule()
                return
            }
            waitingLoading = e0
        }
        val api = api()
        try {
            if (api == null) return
            val seen = waitingTouches.get()
            val list = api.listWaiting()
            if (stopped || epoch.get() != e0) return
            waitingFailed.set(false)
            waitingComplained.set(false)
            // Shown even if an event overtook it (a busy host answers slower than events come); the event scheduled a call,
            // or this one does, to correct it.
            update { withWaiting(it, list) }
            if (seen != waitingTouches.get()) schedule()
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            // The list stays as it was; the next tick asks again.
            if (epoch.get() == e0) {
                waitingFailed.set(true)
                complain(waitingComplained, "list what waits on you", e)
            }
        } finally {
            synchronized(lock) { if (waitingLoading == e0) waitingLoading = -1 }
        }
    }

    /** At most one call per [debounceMs], however many events come: a burst is one call, and a steady stream can't starve it. */
    private fun schedule() {
        synchronized(lock) {
            if (stopped || pause?.isActive == true) return
            pause = launch {
                delay(debounceMs)
                synchronized(lock) { pause = null }
                loadWaiting()
            }
        }
    }

    private suspend fun loadPullRequests() {
        if (stopped || !ready) return
        val e0 = epoch.get()
        synchronized(lock) {
            if (prsLoading == e0) return
            prsLoading = e0
        }
        val api = api()
        var again = false
        try {
            if (api == null) return
            // The host answers per run (a device names it): one call for each run still open, side by side.
            val runIds = state().runs.values.filter { it.status != RunStatus.RUN_STATUS_CLOSED }.map { it.id }
            val seen = prTouches.get()
            val results = coroutineScope { runIds.map { id -> async { runCatching { api.listPullRequests(id) } } }.awaitAll() }
            if (stopped || epoch.get() != e0) return
            results.firstNotNullOfOrNull { it.exceptionOrNull() as? CancellationException }?.let { throw it }
            if (seen != prTouches.get()) {
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
                if (prsComplained.compareAndSet(false, true)) log("couldn't read the pull requests: ${failed.message ?: failed.javaClass.simpleName}")
            } else prsComplained.set(false)
        } finally {
            synchronized(lock) { if (prsLoading == e0) prsLoading = -1 }
            // Not at once: under a steady stream of PR events that would be one call after another.
            if (again) launch { delay(RETRY_PRS_MS); loadPullRequests() }
        }
    }

    private suspend fun loadAgents() {
        if (stopped || !ready) return
        val api = api() ?: return
        if (waitingFailed.compareAndSet(true, false)) {
            // One call per tick, so no pause is needed.
            waitingTouches.incrementAndGet()
            launch { loadWaiting() }
        }
        val e0 = epoch.get()
        val seen = agentTouches.get()
        try {
            val list = api.listAgents()
            agentsComplained.set(false)
            if (stopped || epoch.get() != e0 || seen != agentTouches.get()) return
            var changed = false
            update { s ->
                // The update may run again if another writer won (a compare and set): count afresh each time.
                changed = false
                val next = s.agents.toMutableMap()
                for (a: Agent in list) if (next[a.id] != a) { next[a.id] = a; changed = true }
                if (changed) s.copy(agents = next) else s
            }
            if (changed) {
                // A prompt aged into unknown, or the other way round, changes what waits.
                waitingTouches.incrementAndGet()
                schedule()
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            complain(agentsComplained, "refresh the agents", e)
        }
    }

    /** Call with every event of the stream, after it was applied to the state. */
    fun event(e: Event) {
        when (e.payloadCase) {
            Event.PayloadCase.MESSAGE_SENT -> {
                val t = e.messageSent.message.type
                if (t == MessageType.MESSAGE_TYPE_PLAN || t == MessageType.MESSAGE_TYPE_QUESTION || t == MessageType.MESSAGE_TYPE_ANSWER) {
                    waitingTouches.incrementAndGet()
                    schedule()
                }
            }
            Event.PayloadCase.AGENT_SPAWNED, Event.PayloadCase.AGENT_STATE_CHANGED, Event.PayloadCase.AGENT_EXITED, Event.PayloadCase.AGENT_RETIRED -> {
                agentTouches.incrementAndGet()
                waitingTouches.incrementAndGet()
                schedule()
            }
            // These change an agent the state holds too, so a list of the agents that was on its way is older than them.
            Event.PayloadCase.AGENT_REOPEN_REQUESTED, Event.PayloadCase.AGENT_REOPEN_DROPPED, Event.PayloadCase.AGENT_REOPENED,
            Event.PayloadCase.AGENT_PHASE_CHANGED, Event.PayloadCase.AGENT_ROLE_CHANGED, Event.PayloadCase.AGENT_ADOPTED -> agentTouches.incrementAndGet()
            Event.PayloadCase.PULL_REQUEST_CHANGED -> prTouches.incrementAndGet()
            Event.PayloadCase.RUN_STOPPED -> {
                waitingTouches.incrementAndGet()
                schedule()
            }
            else -> {}
        }
    }

    /**
     * A snapshot or a resume arrived (the first connection, a drop and a reconnect, or a resync): a new epoch, so what an
     * earlier one had on its way is dropped, and what waits and the pull requests are listed at once.
     */
    fun synced() {
        if (stopped) return
        newEpoch()
        launch { loadWaiting() }
        launch { loadPullRequests() }
    }

    /** The daemon at this address is another one: nothing asked of the old one counts any more. */
    fun reset() {
        newEpoch()
        waitingComplained.set(false)
        agentsComplained.set(false)
        prsComplained.set(false)
        waitingFailed.set(false)
    }

    private fun newEpoch() {
        epoch.incrementAndGet()
        waitingTouches.incrementAndGet()
        agentTouches.incrementAndGet()
        prTouches.incrementAndGet()
        synchronized(lock) { pause?.cancel(); pause = null }
    }

    fun start() {
        synchronized(lock) {
            if (tickers.isNotEmpty()) return
            if (stopped) {
                // Started again after a stop: a new lifetime.
                stopped = false
                life = SupervisorJob(scope.coroutineContext[Job])
            }
        }
        tickers = listOfNotNull(
            launch { while (isActive) { delay(agentsEveryMs); loadAgents() } },
            launch { while (isActive) { delay(prsEveryMs); loadPullRequests() } },
        )
    }

    /** Nothing more is asked after this, and what was on its way is cancelled. */
    fun stop() {
        synchronized(lock) {
            stopped = true
            life.cancel()
            pause = null
            tickers = emptyList()
        }
    }
}
