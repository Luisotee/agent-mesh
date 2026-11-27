package dev.agentmesh.android.chat

import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.Message
import java.text.DateFormat
import java.util.Calendar
import java.util.Date

/**
 * The chat's timeline: what the agent's conversation shows, in order, with the mesh messages to and from it (plans,
 * questions, tasks, reports) standing at their place by time. Pure, so the merge is checked without a screen.
 */
sealed interface Entry {
    val id: String

    /** A block of the CLI's own conversation. */
    data class Chat(val block: Block) : Entry { override val id get() = block.id }

    /** A mesh message, shown as a card. */
    data class Card(val message: Message) : Entry { override val id get() = "msg:${message.id}" }
}

private fun delivered(m: Message): Long = if (m.hasDeliveredAt()) m.deliveredAt.toMillis() else 0L

private val Entry.at: Long
    get() = when (this) {
        is Entry.Chat -> block.at
        is Entry.Card -> if (message.hasCreatedAt()) message.createdAt.toMillis() else 0L
    }

/**
 * [blocks] (in the order of the record) with [messages] (oldest first) put in by time: a message stands before the first
 * block that is later than it, and after the last block when it is later than all of them. A block with no time takes the
 * time of the one before it, so it never jumps. With [cards] (messages are held), the doorbell lines the CLI recorded
 * ("📬 [mesh] … run `mesh inbox`") are dropped: each is one of the cards. Without it they stay, so the chat still says
 * a message came.
 */
fun mergeTimeline(blocks: List<Block>, messages: List<Message>, cards: Boolean, whole: Boolean = true, keep: (Message) -> Boolean = { false }): List<Entry> {
    // A message with no time stands where the one before it does (the first, with the next one's), so it never holds the rest back.
    val cardsAt = messages.map { Entry.Card(it).at }.toMutableList()
    for (i in 1 until cardsAt.size) if (cardsAt[i] == 0L) cardsAt[i] = cardsAt[i - 1]
    for (i in cardsAt.size - 2 downTo 0) if (cardsAt[i] == 0L) cardsAt[i] = cardsAt[i + 1]
    // The chat reads the tail of the record: while earlier lines are still to load ([whole] false), a message older than
    // the first line held waits for them too (unless [keep] says it must show, as one that waits on the user), or the
    // old ones would all pile up above it. A message sent before the window but delivered inside it belongs in it: its doorbell is there.
    val first = blocks.firstOrNull { it.at > 0 }?.at ?: 0L
    val pending = ArrayDeque(messages.mapIndexed { i, m -> Entry.Card(m) to cardsAt[i] }.filter { (c, at) -> whole || first == 0L || at == 0L || at >= first || delivered(c.message) >= first || keep(c.message) })
    val out = ArrayList<Entry>(blocks.size + messages.size)
    var clock = 0L
    for (b in blocks) {
        if (cards && b is Block.Mesh) continue
        clock = maxOf(clock, b.at)
        while (pending.isNotEmpty() && pending.first().second in 1..clock) out.add(pending.removeFirst().first)
        out.add(Entry.Chat(b))
    }
    out.addAll(pending.map { it.first })
    return out
}

/** Where the entry that was on screen is now: by its id, or for a run of tool calls by any of its calls ([indexOfBlock]). -1 when it isn't there. */
fun indexOfEntry(entries: List<Entry>, key: String): Int =
    entries.indexOfFirst { it.id == key || (it is Entry.Chat && it.block is Block.Tools && it.block.calls.any { c -> c.id == key }) }

/** Start a new group of the timeline after this long a pause. */
const val GROUP_GAP_MS = 5 * 60_000L

/**
 * The ids of the entries that carry a time label: the first of the timeline with a time, and each one that comes
 * [GROUP_GAP_MS] or more after the last one labelled, so a quick exchange has one label and a pause starts a new one.
 */
fun stamped(entries: List<Entry>): Set<String> {
    val out = HashSet<String>()
    var last = -1L
    for (e in entries) {
        val at = e.at
        if (at <= 0) continue
        if (last < 0 || at - last >= GROUP_GAP_MS) { out.add(e.id); last = at }
    }
    return out
}

/** When an entry happened, in ms; 0 when it has no time. */
fun entryTime(e: Entry): Long = e.at

/** "just now", "12 min ago", "3 h ago", "Yesterday 14:05", "Mon 14:05" within a week, or a date: a label's wording, by [nowMs] and the zone's calendar. */
fun ago(thenMs: Long, nowMs: Long, zone: java.util.TimeZone = java.util.TimeZone.getDefault(), locale: java.util.Locale = java.util.Locale.getDefault()): String {
    val d = nowMs - thenMs
    val time = DateFormat.getTimeInstance(DateFormat.SHORT, locale).apply { timeZone = zone }.format(Date(thenMs))
    if (d < 60_000) return "just now"
    if (d < 3_600_000) return "${d / 60_000} min ago"
    val a = Calendar.getInstance(zone).apply { timeInMillis = thenMs }
    val b = Calendar.getInstance(zone).apply { timeInMillis = nowMs }
    val days = dayNumber(b) - dayNumber(a)
    return when {
        days == 0L -> "${d / 3_600_000} h ago"
        days == 1L -> "Yesterday $time"
        days < 7 -> "${java.text.SimpleDateFormat("EEE", locale).apply { timeZone = zone }.format(Date(thenMs))} $time"
        else -> DateFormat.getDateInstance(DateFormat.MEDIUM, locale).apply { timeZone = zone }.format(Date(thenMs))
    }
}

private fun dayNumber(c: Calendar): Long = java.time.Instant.ofEpochMilli(c.timeInMillis).atZone(c.timeZone.toZoneId()).toLocalDate().toEpochDay()

/** [text] as a Markdown quote to open a reply with: each line marked, then a blank line to type under. */
fun quoteReply(text: String): String = text.trim().lines().joinToString("\n") { "> $it".trimEnd() } + "\n\n"
