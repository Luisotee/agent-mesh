package dev.agentmesh.android.chat

import dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse
import dev.agentmesh.gen.agentmesh.v1.TranscriptEntry
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind

/**
 * What the chat view holds of an agent's conversation: the entries read so far (a window of its CLI's record) and where
 * that window begins and ends, so it can read earlier ones and follow new ones. Pure functions over it, so what reading
 * back, following and a rewritten record do is checked without a screen. The desktop's `transcript.ts`, with its rules.
 */
data class Transcript(
    /** False when the host reads no record for this CLI. */
    val supported: Boolean = true,
    val sessionId: String = "",
    val entries: List<TranscriptEntry> = emptyList(),
    val start: Long = 0,
    val end: Long = 0,
    val atStart: Boolean = false,
)

/** The first read: the tail of the record. It replaces whatever was held. */
fun fromTail(res: ReadTranscriptResponse) =
    Transcript(res.supported, res.sessionId, res.entriesList, res.startOffset, res.endOffset, res.atStart)

/** The entries of a page that aren't held already: an entry shows once (a list needs unique keys), however a record is re-read. */
private fun fresh(t: Transcript, res: ReadTranscriptResponse): List<TranscriptEntry> {
    val held = t.entries.mapTo(HashSet()) { it.id }
    return res.entriesList.filter { held.add(it.id) }
}

/** Earlier entries, put before the ones held. */
fun withEarlier(t: Transcript, res: ReadTranscriptResponse): Transcript {
    // A page of another conversation, or one that doesn't end where the held entries begin (the record was rewritten or
    // reloaded while it was out), would repeat or misplace entries.
    if (res.sessionId != t.sessionId || res.endOffset != t.start) return t
    return t.copy(entries = fresh(t, res) + t.entries, start = res.startOffset, atStart = res.atStart)
}

sealed interface Followed {
    data class More(val transcript: Transcript) : Followed
    data object Same : Followed
    data object Reload : Followed
}

/**
 * Entries after the ones held, from a read that began at [from] (what the transcript held when it was asked: earlier
 * entries may have come in above since, so [t] is what is held now). A record that is another conversation now (a
 * reopen, `/clear`) or that ends before where we had read (it was rewritten) can't be followed: the view reads the tail
 * again. A read that began somewhere other than where the transcript ends now is not appended: it is [Followed.Same],
 * and the next read starts from the right place.
 */
fun withLater(t: Transcript, res: ReadTranscriptResponse, from: Long = t.end): Followed {
    if (res.sessionId != t.sessionId || res.endOffset < t.end) return Followed.Reload
    if (from != t.end) return Followed.Same
    if (res.entriesCount == 0 && res.endOffset == t.end) return Followed.Same
    return Followed.More(t.copy(entries = t.entries + fresh(t, res), end = res.endOffset))
}

/** What the chat shows, in order: a prompt, a mesh doorbell, a reply, or a run of tool calls with their answers. */
sealed interface Block {
    val id: String

    data class User(override val id: String, val entry: TranscriptEntry) : Block
    data class Mesh(override val id: String, val entry: TranscriptEntry) : Block
    data class Assistant(override val id: String, val entry: TranscriptEntry) : Block
    data class Tools(override val id: String, val calls: List<ToolCall>) : Block
}

class ToolCall(
    val id: String,
    val tool: String,
    val input: String,
    /** What the tool answered; null until it did. */
    var result: TranscriptEntry? = null,
)

/** Entries as blocks: consecutive tool calls fold into one block, each call with the result that carries its id. */
fun toBlocks(entries: List<TranscriptEntry>): List<Block> {
    val blocks = ArrayList<Block>()
    val calls = HashMap<String, ToolCall>()
    var run: MutableList<ToolCall>? = null
    fun tools(id: String): MutableList<ToolCall> {
        val r = run
        if (r != null) return r
        val list = ArrayList<ToolCall>()
        blocks.add(Block.Tools(id, list))
        run = list
        return list
    }
    for (e in entries) {
        when (e.kind) {
            TranscriptKind.TRANSCRIPT_KIND_TOOL_CALL -> {
                val call = ToolCall(e.id, e.tool, e.toolInput)
                calls[e.toolUseId] = call
                tools(e.id).add(call)
            }
            TranscriptKind.TRANSCRIPT_KIND_TOOL_RESULT -> {
                val call = calls[e.toolUseId]
                // A result whose call is before the window we hold: shown on its own, as a call we can't name.
                if (call != null) call.result = e else tools(e.id).add(ToolCall(e.id, "tool", "", e))
            }
            TranscriptKind.TRANSCRIPT_KIND_USER -> { run = null; blocks.add(Block.User(e.id, e)) }
            TranscriptKind.TRANSCRIPT_KIND_MESH -> { run = null; blocks.add(Block.Mesh(e.id, e)) }
            TranscriptKind.TRANSCRIPT_KIND_ASSISTANT -> { run = null; blocks.add(Block.Assistant(e.id, e)) }
            else -> {}
        }
    }
    return blocks
}

/** "Ran 3 tools: Bash ×2, Read", for the folded block. */
fun toolsSummary(calls: List<ToolCall>): String {
    val counts = LinkedHashMap<String, Int>()
    for (c in calls) counts[c.tool] = (counts[c.tool] ?: 0) + 1
    val names = counts.entries.joinToString(", ") { (t, n) -> if (n > 1) "$t ×$n" else t }
    return "Ran ${calls.size} ${if (calls.size == 1) "tool" else "tools"}: $names"
}

/**
 * Where the block that was on screen is now: by its own id, or, for a run of tool calls, by any of its calls' ids: earlier
 * entries joined to the run (a page that ends with a tool call before a run that begins with one) give the whole run the
 * id of its new first call. -1 when it isn't there.
 */
fun indexOfBlock(blocks: List<Block>, key: String): Int =
    blocks.indexOfFirst { it.id == key || (it is Block.Tools && it.calls.any { c -> c.id == key }) }
