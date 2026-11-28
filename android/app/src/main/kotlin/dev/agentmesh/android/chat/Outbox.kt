package dev.agentmesh.android.chat

import org.json.JSONArray
import org.json.JSONObject

/**
 * What became of a prompt the user sent from the chat, before the host's own copy (the queue, or the record) shows it.
 * The desktop shows "sending…" for it; here it is also kept while the phone has no connection, and when the host refused.
 */
enum class OutState {
    /** The call is out. */
    Sending,

    /** No connection: sent as soon as there is one (the user can take it back meanwhile). */
    Waiting,

    /** The host typed it at once: it is in the agent's terminal and shows in the record in a moment. */
    Sent,

    /** The host refused it, or it could not be sent: Retry, edit it, or drop it. */
    Failed,
}

/** Where the unsent prompts of a host are kept across a restart (the device's preferences). */
interface OutboxStore {
    fun load(hostId: String): String?
    fun save(hostId: String, json: String)
}

/** What the chat held when a prompt was sent: the user prompts by id, and the last block (the desktop's `Seen`). */
data class Seen(val users: Set<String>, val last: String?)

class Outgoing(
    val id: String,
    val agentId: String,
    val text: String,
    /** When the user sent it (the phone's clock). */
    val atMs: Long,
    val state: OutState,
    /** Why it failed, in the host's words. */
    val error: String = "",
    /** When it was typed ([OutState.Sent]): the record has until [SENT_SHOWN_MS] after that to show it. */
    val sentMs: Long = 0,
    val seen: Seen? = null,
) {
    fun with(state: OutState = this.state, error: String = this.error, sentMs: Long = this.sentMs, seen: Seen? = this.seen) =
        Outgoing(id, agentId, text, atMs, state, error, sentMs, seen)
}

/** A bubble the record never shows (the prompt went nowhere) goes after this long. */
const val SENT_SHOWN_MS = 30_000L

fun snapshotOf(blocks: List<Block>) = Seen(blocks.filterIsInstance<Block.User>().map { it.id }.toSet(), blocks.lastOrNull()?.id)

/**
 * The [OutState.Sent] prompts that are still to be shown: each new user prompt that comes AFTER the last block of the
 * snapshot ends one (whatever the CLI echoes of its text), and is spent, so the prompts left do not take it for theirs. One
 * with no snapshot yet waits for one, and one the record never shows goes after [SENT_SHOWN_MS]. The desktop's rule.
 */
fun settleSent(list: List<Outgoing>, blocks: List<Block>, nowMs: Long): List<Outgoing> {
    var out = list
    val first = out.firstOrNull { it.state == OutState.Sent && it.seen != null }?.seen
    if (first != null) {
        val from = first.last?.let { id -> blocks.indexOfFirst { it.id == id } } ?: -1
        val fresh = blocks.drop(from + 1).filterIsInstance<Block.User>().map { it.id }.filter { it !in first.users }
        if (fresh.isNotEmpty()) {
            var drop = fresh.size
            val spent = fresh.toSet()
            out = out.filter { !(it.state == OutState.Sent && it.seen != null && drop-- > 0) }
                .map { if (it.state == OutState.Sent && it.seen != null) it.with(seen = Seen(it.seen.users + spent, it.seen.last)) else it }
        }
    }
    return out.filter { it.state != OutState.Sent || nowMs - it.sentMs < SENT_SHOWN_MS }
}

/** A prompt as kept on the device while it is unsent (not [OutState.Sent]: the host has it). */
fun outgoingToJson(list: List<Outgoing>): String {
    val arr = JSONArray()
    for (o in list) if (o.state != OutState.Sent) arr.put(JSONObject().put("id", o.id).put("agent", o.agentId).put("text", o.text).put("at", o.atMs).put("state", o.state.name).put("error", o.error))
    return arr.toString()
}

/**
 * What a restart makes of the kept prompts: one that was Waiting is Waiting still, and one that was out
 * ([OutState.Sending]) is Failed: the host may or may not have it, and the user decides.
 */
fun outgoingFromJson(json: String?): List<Outgoing> = runCatching {
    val arr = JSONArray(json ?: return emptyList())
    (0 until arr.length()).map { arr.getJSONObject(it) }.mapNotNull { o ->
        val text = o.getString("text")
        if (text.isBlank()) return@mapNotNull null
        val was = runCatching { OutState.valueOf(o.getString("state")) }.getOrDefault(OutState.Failed)
        val (state, error) = when (was) {
            OutState.Waiting -> OutState.Waiting to ""
            OutState.Sending -> OutState.Failed to "The app closed while this was being sent: look in the chat before sending it again."
            else -> OutState.Failed to o.optString("error")
        }
        Outgoing(o.getString("id"), o.getString("agent"), text, o.getLong("at"), state, error)
    }
}.getOrDefault(emptyList())
