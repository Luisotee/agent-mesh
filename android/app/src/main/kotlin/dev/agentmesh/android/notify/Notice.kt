package dev.agentmesh.android.notify

import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.Event

/** What a notification is about; the host's `app.notify.events` setting names these (docs/apps.md, "Notifications"). */
enum class NoticeKind(val key: String) {
    Input("input"), Plan("plan"), Question("question"), Report("report"), Failed("failed"), Exited("exited"), Limited("limited"), Error("error"),
    Finished("finished"), Ready("ready"),
}

/** The screen a notification opens. */
enum class Tab { Overview, Chat, Messages, Terminal }

class Notice(
    val kind: NoticeKind,
    val title: String,
    val body: String,
    val agentId: String,
    val tab: Tab,
    /** The host's event this came from (0: none): the same event from the live stream and from a push posts once. */
    val seq: Long = 0,
    /** A later notice with the same tag replaces the earlier (one per agent, or one per run). */
    val tag: String = "",
)

/** An event older than this wasn't "just now": history replayed after a reconnect doesn't notify. */
const val LIVE_WITHIN_MS = 60_000L

private const val MAX_BODY = 200
private const val MAX_SCAN = 4_000

private val LINE_MARK = Regex("""^\s*(#{1,6}\s+|>\s*|[-*+]\s+|\d+\.\s+)""")
private val FENCE = Regex("""^\s*```\w*\s*$""")
// Emphasis and code in pairs only: a lone `*` or backtick ("does 2*3 equal 6?") is the author's own character.
private val BOLD_ITALIC = Regex("""(?<![\w*])(\*{1,3})(?=\S)(.+?)(?<=\S)\1(?![\w*])""")
private val CODE = Regex("""`([^`\n]+)`""")

/** A notification is plain text: drop the Markdown's own marks (headings, bullets, paired emphasis and code ticks). */
fun brief(text: String): String {
    // A notification shows 200 characters: a huge body isn't scanned whole (the marks are stripped from a few KB).
    val flat = text.take(MAX_SCAN).lines().filterNot { FENCE.matches(it) }.joinToString(" ") { it.replace(LINE_MARK, "") }
        .replace(CODE, "$1").replace(BOLD_ITALIC, "$2").replace(Regex("\\s+"), " ").trim()
    return if (flat.length > MAX_BODY) flat.take(MAX_BODY - 1) + "…" else flat
}

/**
 * What an event means to the person: only the host's own decision, a `UserNotice` event (the daemon's `internal/notice`
 * decides once; the phones' pushes, the desktop's notifications and the Activity list follow it). It is shown only when it
 * is for the user (`for_user`) and live: history replayed after a reconnect stays quiet, and a notice of a kind this app
 * doesn't know (a newer daemon's) is the Activity list's alone. [clockSkewMs] is how far the host's clock is ahead of ours.
 */
fun noticeFor(e: Event, nowMs: Long, clockSkewMs: Long = 0): Notice? {
    if (e.payloadCase != Event.PayloadCase.USER_NOTICE || !e.userNotice.forUser) return null
    val at = if (e.hasTime()) e.time.toMillis() else 0L
    // The event's time is the host's: compare it with the host's clock now, not ours.
    if (nowMs + clockSkewMs - at > LIVE_WITHIN_MS) return null
    val n = e.userNotice
    val kind = NoticeKind.entries.firstOrNull { it.key == n.kind } ?: return null
    if (n.agentId.isEmpty()) return null
    return Notice(kind, n.title, n.body, n.agentId, if (n.tab == "terminal") Tab.Terminal else Tab.Chat, e.seq, n.tag.ifEmpty { n.agentId })
}
