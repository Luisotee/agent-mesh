package dev.agentmesh.android.store

import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.gen.agentmesh.v1.Run
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.util.concurrent.ConcurrentHashMap

/**
 * Loads the stored messages of a host's open runs into its state, a page at a time: the event stream starts from a
 * snapshot, so what was said before the app connected comes only from `ListMessages`. A run that loaded completely
 * once resumes after its newest message on a later snapshot; one that didn't starts over. What waits on the user is
 * derived from the history, so a failed load is tried again, a few times, with a growing wait (the desktop's
 * `messages.ts`).
 */
class MessageLoader(
    private val scope: CoroutineScope,
    private val api: () -> MeshApi?,
    private val state: () -> HostState,
    private val update: ((HostState) -> HostState) -> Unit,
    private val log: (String) -> Unit,
    private val retryBaseMs: Long = 1_000,
) {
    private val complete = ConcurrentHashMap.newKeySet<String>()
    private val running = ConcurrentHashMap.newKeySet<String>()
    private val retries = ConcurrentHashMap<String, Int>()

    /** Load the open runs of a snapshot. */
    fun load(runs: Collection<Run>) {
        for (r in runs) if (r.status == RunStatus.RUN_STATUS_OPEN) scope.launch { loadRun(r.id) }
    }

    private suspend fun loadRun(runId: String) {
        if (!running.add(runId)) return
        try {
            val client = api() ?: error("not connected")
            var after = ""
            if (runId in complete) {
                after = state().messages.values.filter { it.runId == runId }.maxWithOrNull(MessageOrder)?.id.orEmpty()
            }
            while (true) {
                val page = client.listMessages(runId, after)
                update { mergeMessages(it, page.messagesList) }
                val last = page.messagesList.lastOrNull()
                if (!page.more || last == null) break
                after = last.id
            }
            complete.add(runId)
            retries.remove(runId)
        } catch (e: kotlinx.coroutines.CancellationException) {
            throw e
        } catch (e: Exception) {
            log("couldn't load the messages of run $runId: ${e.message}")
            val n = retries[runId] ?: 0
            if (n < MAX_RETRIES) {
                retries[runId] = n + 1
                running.remove(runId)
                scope.launch { delay(retryBaseMs * (1L shl n)); loadRun(runId) }
                return
            }
        } finally {
            running.remove(runId)
        }
    }

    private companion object {
        const val MAX_RETRIES = 5
    }
}
