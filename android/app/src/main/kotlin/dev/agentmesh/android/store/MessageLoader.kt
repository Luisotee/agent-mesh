package dev.agentmesh.android.store

import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.gen.agentmesh.v1.Run
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.Job
import java.util.concurrent.ConcurrentHashMap

/**
 * Loads the stored messages of a host's open runs into its state, a page at a time: the event stream starts from a
 * snapshot, so what was said before the app connected comes only from `ListMessages`. A run records how far its
 * listing got (the last message of its last page, not whatever an event added meanwhile), and a later snapshot
 * resumes from there, so a message sent during a gap is fetched even when a live one reached us first. The state
 * says when a run's history is complete on this connection ([HostState.messagesLoaded]) or couldn't be loaded
 * ([HostState.messagesFailed]). What waits on the user is derived from the history, so a failed load is tried
 * again, a few times, with a growing wait (the desktop's `messages.ts`).
 */
class MessageLoader(
    private val scope: CoroutineScope,
    private val api: () -> MeshApi?,
    private val state: () -> HostState,
    private val update: ((HostState) -> HostState) -> Unit,
    private val log: (String) -> Unit,
    private val retryBaseMs: Long = 1_000,
) {
    /** Per run: the last message of its last completely listed page. */
    private val listedTo = ConcurrentHashMap<String, String>()
    private val running = ConcurrentHashMap.newKeySet<String>()
    private val retries = ConcurrentHashMap<String, Int>()
    private val pending = ConcurrentHashMap<String, Job>()

    /** Load the open runs of a snapshot. */
    fun load(runs: Collection<Run>) {
        for (r in runs) if (r.status == RunStatus.RUN_STATUS_OPEN) {
            // A new request (a new snapshot) starts the retries over, and replaces one waiting to fire: the host may be fine now.
            retries.remove(r.id)
            pending.remove(r.id)?.cancel()
            update { it.copy(messagesLoaded = it.messagesLoaded - r.id, messagesFailed = it.messagesFailed - r.id) }
            scope.launch { loadRun(r.id) }
        }
    }

    /** Forget everything: another daemon answers at this address, and its run ids mean something else. */
    fun reset() {
        listedTo.clear(); retries.clear(); running.clear()
        pending.values.forEach { it.cancel() }; pending.clear()
    }

    private suspend fun loadRun(runId: String) {
        if (!running.add(runId)) return
        try {
            val client = api() ?: error("not connected")
            var after = listedTo[runId].orEmpty()
            while (true) {
                val page = client.listMessages(runId, after)
                update { mergeMessages(it, page.messagesList) }
                val last = page.messagesList.lastOrNull()
                if (last != null) { after = last.id; listedTo[runId] = after }
                if (!page.more || last == null) break
            }
            retries.remove(runId)
            update { it.copy(messagesLoaded = it.messagesLoaded + runId, messagesFailed = it.messagesFailed - runId) }
        } catch (e: kotlinx.coroutines.CancellationException) {
            throw e
        } catch (e: Exception) {
            log("couldn't load the messages of run $runId: ${e.message}")
            val n = retries[runId] ?: 0
            if (n < MAX_RETRIES) {
                retries[runId] = n + 1
                running.remove(runId)
                pending[runId] = scope.launch { delay(retryBaseMs * (1L shl n)); pending.remove(runId); loadRun(runId) }
                return
            }
            update { it.copy(messagesFailed = it.messagesFailed + runId) }
        } finally {
            running.remove(runId)
        }
    }

    private companion object {
        const val MAX_RETRIES = 5
    }
}
