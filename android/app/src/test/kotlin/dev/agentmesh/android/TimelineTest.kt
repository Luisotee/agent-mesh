package dev.agentmesh.android

import dev.agentmesh.android.chat.Block
import dev.agentmesh.android.chat.Entry
import dev.agentmesh.android.chat.ago
import dev.agentmesh.android.chat.mergeTimeline
import dev.agentmesh.android.chat.stamped
import dev.agentmesh.android.chat.toBlocks
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.TranscriptEntry
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Locale
import java.util.TimeZone

/** The chat's timeline: mesh messages at their place by time, the doorbells they replace, the time labels. */
class TimelineTest {
    private fun e(id: String, kind: TranscriptKind, atMs: Long?, text: String = ""): TranscriptEntry =
        TranscriptEntry.newBuilder().setId(id).setKind(kind).setText(text).apply { if (atMs != null) setTime(com.google.protobuf.Timestamp.newBuilder().setSeconds(atMs / 1000).build()) }.build()

    private fun user(id: String, at: Long?) = e(id, TranscriptKind.TRANSCRIPT_KIND_USER, at, "hi")
    private fun reply(id: String, at: Long?) = e(id, TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, at, "ok")
    private fun bell(id: String, at: Long?) = e(id, TranscriptKind.TRANSCRIPT_KIND_MESH, at, "📬 [mesh] a plan from w1: run `mesh inbox`")
    private fun plan(id: String, at: Long) = message(id, from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, at = at)
    private fun ids(l: List<Entry>) = l.map { it.id }

    @Test fun `a message stands before the first block that is later and after the earlier ones`() {
        val blocks = toBlocks(listOf(user("u1", 10_000), reply("a1", 20_000), user("u2", 40_000)))
        val out = mergeTimeline(blocks, listOf(plan("p1", 30_000)), cards = true)
        assertEquals(listOf("u1", "a1", "msg:p1", "u2"), ids(out))
    }

    @Test fun `a message later than everything goes last and one earlier than everything goes first`() {
        val blocks = toBlocks(listOf(user("u1", 10_000)))
        assertEquals(listOf("msg:p0", "u1", "msg:p2"), ids(mergeTimeline(blocks, listOf(plan("p0", 5_000), plan("p2", 99_000)), cards = true)))
    }

    @Test fun `messages keep their order, and with no blocks they are the timeline`() {
        assertEquals(listOf("msg:a", "msg:b"), ids(mergeTimeline(emptyList(), listOf(plan("a", 1_000), plan("b", 2_000)), cards = true)))
    }

    @Test fun `doorbells give way to the cards once the messages are known, and stay until then`() {
        val blocks = toBlocks(listOf(user("u1", 10_000), bell("b1", 20_000), reply("a1", 30_000)))
        assertEquals(listOf("u1", "msg:p1", "a1"), ids(mergeTimeline(blocks, listOf(plan("p1", 20_000)), cards = true)))
        assertEquals(listOf("u1", "b1", "a1"), ids(mergeTimeline(blocks, emptyList(), cards = false)))
    }

    @Test fun `a block with no time takes the one before it, so a message does not jump over it`() {
        val blocks = toBlocks(listOf(user("u1", 10_000), reply("a1", null), user("u2", 50_000)))
        assertEquals(listOf("u1", "a1", "msg:p1", "u2"), ids(mergeTimeline(blocks, listOf(plan("p1", 30_000)), cards = true)))
    }

    @Test fun `a message with no time is kept, after the blocks`() {
        val noTime = dev.agentmesh.gen.agentmesh.v1.Message.newBuilder().setId("n").setToId("lead").setType(MessageType.MESSAGE_TYPE_INFO).build()
        assertEquals(listOf("u1", "msg:n"), ids(mergeTimeline(toBlocks(listOf(user("u1", 10_000))), listOf(noTime), cards = true)))
    }

    @Test fun `a message with no time stands where the one before it does and holds nothing back`() {
        val noTime = dev.agentmesh.gen.agentmesh.v1.Message.newBuilder().setId("n").setToId("lead").setType(MessageType.MESSAGE_TYPE_INFO).build()
        val blocks = toBlocks(listOf(user("u1", 5_000), user("u2", 15_000), user("u3", 25_000)))
        assertEquals(listOf("u1", "msg:a", "msg:n", "u2", "msg:c", "u3"), ids(mergeTimeline(blocks, listOf(plan("a", 10_000), noTime, plan("c", 20_000)), cards = true)))
    }

    @Test fun `a time label opens the timeline and each group after a pause of five minutes`() {
        val m = 60_000L
        val blocks = toBlocks(listOf(user("u1", 0 + m), reply("a1", 2 * m), user("u2", 10 * m), reply("a2", 11 * m), user("u3", 30 * m)))
        val out = mergeTimeline(blocks, emptyList(), cards = true)
        assertEquals(setOf("u1", "u2", "u3"), stamped(out))
        assertTrue(stamped(emptyList()).isEmpty())
    }

    @Test fun `an entry with no time never takes a label`() {
        val out = mergeTimeline(toBlocks(listOf(user("u1", null), reply("a1", 60_000))), emptyList(), cards = true)
        assertEquals(setOf("a1"), stamped(out))
    }

    @Test fun `says how long ago, by the calendar day`() {
        val utc = TimeZone.getTimeZone("UTC")
        val now = 1_800_000_000_000L // a fixed noon-ish instant
        fun at(d: Long) = ago(now - d, now, utc, Locale.US)
        assertEquals("just now", at(30_000))
        assertEquals("12 min ago", at(12 * 60_000))
        assertEquals("3 h ago", at(3 * 3_600_000))
        assertTrue(at(26 * 3_600_000), at(26 * 3_600_000).startsWith("Yesterday"))
        assertTrue(at(3 * 86_400_000L).matches(Regex("[A-Z][a-z]{2} .*")))
        assertTrue(at(30 * 86_400_000L).contains(","))
    }

    @Test fun `yesterday across New Year's Eve is yesterday`() {
        val utc = TimeZone.getTimeZone("UTC")
        fun ms(y: Int, m: Int, d: Int, h: Int) = java.time.ZonedDateTime.of(y, m, d, h, 0, 0, 0, java.time.ZoneOffset.UTC).toInstant().toEpochMilli()
        assertTrue(ago(ms(2026, 12, 31, 22), ms(2027, 1, 1, 10), utc, Locale.US).startsWith("Yesterday"))
        assertTrue(ago(ms(2026, 12, 26, 22), ms(2027, 1, 1, 10), utc, Locale.US).matches(Regex("[A-Z][a-z]{2} .*")))
    }

    @Test fun `while earlier lines are still to load, older messages wait for them, except one that waits on the user`() {
        val blocks = toBlocks(listOf(user("u1", 50_000), user("u2", 60_000)))
        val old = listOf(plan("p0", 10_000), plan("p1", 20_000), plan("p2", 55_000))
        assertEquals(listOf("u1", "msg:p2", "u2"), ids(mergeTimeline(blocks, old, cards = true, whole = false)))
        assertEquals(listOf("msg:p1", "u1", "msg:p2", "u2"), ids(mergeTimeline(blocks, old, cards = true, whole = false, keep = { it.id == "p1" })))
        assertEquals(listOf("msg:p0", "msg:p1", "u1", "msg:p2", "u2"), ids(mergeTimeline(blocks, old, cards = true)))
    }

    @Test fun `a message sent before the window but delivered inside it stays, and one with no time is not held back`() {
        val blocks = toBlocks(listOf(user("u1", 100_000), user("u2", 110_000)))
        val late = plan("late", 99_000).toBuilder().setDeliveredAt(com.google.protobuf.Timestamp.newBuilder().setSeconds(105).build()).build()
        assertEquals(listOf("msg:late", "u1", "u2"), ids(mergeTimeline(blocks, listOf(plan("old", 10_000), late), cards = true, whole = false)))
        val noTime = dev.agentmesh.gen.agentmesh.v1.Message.newBuilder().setId("n").setToId("lead").setType(MessageType.MESSAGE_TYPE_INFO).build()
        assertEquals(listOf("u1", "u2", "msg:n"), ids(mergeTimeline(blocks, listOf(noTime), cards = true, whole = false)))
    }
}
