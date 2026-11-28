package dev.agentmesh.android

import dev.agentmesh.android.chat.Block
import dev.agentmesh.android.chat.OutState
import dev.agentmesh.android.chat.Outgoing
import dev.agentmesh.android.chat.SENT_SHOWN_MS
import dev.agentmesh.android.chat.Seen
import dev.agentmesh.android.chat.outgoingFromJson
import dev.agentmesh.android.chat.outgoingToJson
import dev.agentmesh.android.chat.settleSent
import dev.agentmesh.android.chat.snapshotOf
import dev.agentmesh.gen.agentmesh.v1.TranscriptEntry
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36])
class OutboxTest {
    private fun user(id: String) = Block.User(id, TranscriptEntry.newBuilder().setId(id).setKind(TranscriptKind.TRANSCRIPT_KIND_USER).setText("t").build())
    private fun reply(id: String) = Block.Assistant(id, TranscriptEntry.newBuilder().setId(id).setKind(TranscriptKind.TRANSCRIPT_KIND_ASSISTANT).setText("r").build())
    private fun sent(id: String, seen: Seen?, at: Long = 1_000) = Outgoing(id, "w1", id, 0, OutState.Sent, sentMs = at, seen = seen)

    @Test fun `a new user prompt after the snapshot ends one sent bubble, and is spent for the rest`() {
        val before = listOf(user("u1"), reply("a1"))
        val snap = snapshotOf(before)
        val list = listOf(sent("x", snap), sent("y", snap))
        // One new prompt in the record ends one bubble only.
        val now = settleSent(list, before + user("u2"), 2_000)
        assertEquals(listOf("y"), now.map { it.id })
        // The bubble left does not take the same prompt for its own; the next one ends it.
        assertEquals(listOf("y"), settleSent(now, before + user("u2"), 2_500).map { it.id })
        assertEquals(emptyList<String>(), settleSent(now, before + user("u2") + reply("a2") + user("u3"), 3_000).map { it.id })
    }

    @Test fun `an older prompt loaded above does not end a bubble, and one without a snapshot waits`() {
        val before = listOf(user("u1"), reply("a1"))
        val snap = snapshotOf(before)
        // An earlier page: u0 comes in before the snapshot's last block.
        assertEquals(1, settleSent(listOf(sent("x", snap)), listOf(user("u0")) + before, 2_000).size)
        assertEquals(1, settleSent(listOf(sent("x", null)), before + user("u2"), 2_000).size)
    }

    @Test fun `a typed prompt the record never shows goes after 30 seconds, others are kept`() {
        val snap = snapshotOf(emptyList())
        val list = listOf(sent("x", snap, at = 1_000), Outgoing("w", "w1", "w", 0, OutState.Waiting))
        assertEquals(listOf("x", "w"), settleSent(list, emptyList(), 1_000 + SENT_SHOWN_MS - 1).map { it.id })
        assertEquals(listOf("w"), settleSent(list, emptyList(), 1_000 + SENT_SHOWN_MS).map { it.id })
    }

    @Test fun `what is unsent is kept across a restart, a waiting one stays waiting, one that was out is failed, a typed one is not kept`() {
        val json = outgoingToJson(listOf(
            Outgoing("a", "w1", "waits", 1, OutState.Waiting),
            Outgoing("b", "w1", "was out", 2, OutState.Sending),
            Outgoing("c", "w1", "refused", 3, OutState.Failed, error = "no box"),
            sent("d", null),
        ))
        val back = outgoingFromJson(json)
        assertEquals(listOf("a", "b", "c"), back.map { it.id })
        assertEquals(listOf(OutState.Waiting, OutState.Failed, OutState.Failed), back.map { it.state })
        assertTrue(back[1].error.contains("look in the chat"))
        assertEquals("no box", back[2].error)
        assertEquals(emptyList<Outgoing>(), outgoingFromJson("not json"))
        assertEquals(emptyList<Outgoing>(), outgoingFromJson(null))
    }
}
