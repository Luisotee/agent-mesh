package dev.agentmesh.android.chat

import dev.agentmesh.android.net.MeshApi
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** What the chat shows of one agent's conversation. */
data class ChatState(
    val transcript: Transcript = Transcript(),
    /** The first read answered. */
    val loaded: Boolean = false,
    val error: String = "",
    val earlierBusy: Boolean = false,
)

/** How often the chat reads on while the agent works (a reply fires no event of its own; a tool call or a state change does). */
const val POLL_MS = 2_000L

/** How long after an event the chat reads on: a burst of events is one read. */
const val SETTLE_MS = 300L

/** How many of a page's replies (the newest) are prepared before it is shown; the rest are parsed as their rows come up. */
const val WARM_ENTRIES = 30

/** A host's refusal as its words, without the code in front of it. */
fun say(e: Throwable): String = (e.message ?: e.javaClass.simpleName).replace(Regex("^\\[[a-z_]+]\\s*"), "")

/**
 * An agent's conversation as its CLI recorded it, kept up to date: the tail first, earlier entries on request, newer
 * ones when told there is more ([sync]). The desktop's `useTranscript`, with its rules: one read at a time (one asked
 * for meanwhile follows it), a read of an earlier conversation is dropped, a record that was rewritten is read again
 * from its tail. [reset] starts over when the conversation changes (a reopen, `/clear`).
 */
class ChatSync(
    private val scope: CoroutineScope,
    private val api: () -> MeshApi?,
    private val agentId: String,
    /** Gets the texts of a page ready before it is shown (the screen parses their Markdown off the main thread), so rows have their height when composed. */
    private val warm: suspend (List<String>) -> Unit = {},
) {
    private val _state = MutableStateFlow(ChatState())
    val state: StateFlow<ChatState> get() = _state

    private var busy = false
    private var again = false
    private var gen = 0
    private var timers: List<Job> = emptyList()

    /** Another conversation (or none yet): what was held is not it. */
    fun reset() {
        gen++
        busy = false
        again = false
        _state.value = ChatState()
    }

    fun sync() {
        if (busy) {
            again = true
            return
        }
        scope.launch { run() }
    }

    private suspend fun warmed(res: dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse) {
        // A failure to prepare is never a failure to show.
        try { warm(res.entriesList.filter { it.kind == dev.agentmesh.gen.agentmesh.v1.TranscriptKind.TRANSCRIPT_KIND_ASSISTANT }.takeLast(WARM_ENTRIES).reversed().map { it.text }) } catch (e: CancellationException) { throw e } catch (e: Exception) { }
    }

    private suspend fun run() {
        if (busy) { again = true; return }
        val api = api() ?: return
        busy = true
        var mine = gen
        try {
            do {
                again = false
                val held = _state.value
                val from = held.transcript.end
                if (!held.loaded) {
                    val res = api.readTranscript(agentId, -1, true)
                    if (mine != gen) return
                    warmed(res)
                    if (mine != gen) return
                    _state.value = held.copy(transcript = fromTail(res), loaded = true, error = "")
                } else {
                    val res = api.readTranscript(agentId, from, false)
                    if (mine != gen) return
                    warmed(res)
                    if (mine != gen) return
                    // What is held now, not what was when the read began: "Load earlier" may have put entries above since.
                    when (val next = withLater(_state.value.transcript, res, from)) {
                        Followed.Reload -> {
                            // A new conversation view: a "Load earlier" still out belongs to the old one.
                            gen++
                            mine = gen
                            _state.value = ChatState()
                            again = true
                        }
                        is Followed.More -> _state.update { it.copy(transcript = next.transcript, error = "") }
                        Followed.Same -> {
                            // The read began somewhere else than where the transcript ends now: read again from the right place.
                            if (from != _state.value.transcript.end) again = true
                            _state.update { it.copy(error = "") }
                        }
                    }
                }
            } while (again)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            if (mine == gen) _state.update { it.copy(error = say(e)) }
        } finally {
            // A read of an earlier conversation ends without freeing the flag: the reset did, and a newer read may hold it now.
            if (mine == gen) busy = false
        }
    }

    fun loadEarlier() {
        val held = _state.value
        if (held.earlierBusy || held.transcript.atStart || !held.loaded) return
        val api = api() ?: return
        val mine = gen
        _state.update { it.copy(earlierBusy = true) }
        scope.launch {
            try {
                val res = api.readTranscript(agentId, held.transcript.start, true)
                // The conversation was reset while this read was out: it is another one's earlier part.
                if (mine != gen) return@launch
                warmed(res)
                if (mine != gen) return@launch
                val cur = _state.value.transcript
                val next = withEarlier(cur, res)
                if (next === cur) {
                    // The page doesn't join what is held (the record changed while it was out): read the tail again.
                    _state.value = ChatState()
                    sync()
                } else _state.update { it.copy(transcript = next) }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                if (mine == gen) _state.update { it.copy(error = say(e)) }
            } finally {
                if (mine == gen) _state.update { it.copy(earlierBusy = false) }
            }
        }
    }

    /** Read on shortly after a prompt is sent (its turn shows within a second or so). */
    fun afterSend() {
        timers.forEach { it.cancel() }
        timers = listOf(300L, 1500L).map { ms -> scope.launch { delay(ms); sync() } }
    }

    fun stop() {
        gen++
        busy = false
        again = false
        _state.update { it.copy(earlierBusy = false) }
        timers.forEach { it.cancel() }
        timers = emptyList()
    }
}
