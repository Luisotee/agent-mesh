package dev.agentmesh.android

import dev.agentmesh.android.chat.ChatSync
import dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse
import dev.agentmesh.gen.agentmesh.v1.TranscriptEntry
import dev.agentmesh.gen.agentmesh.v1.TranscriptKind
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** How the chat reads an agent's conversation: the desktop's `useTranscript`. */
@OptIn(ExperimentalCoroutinesApi::class)
class ChatSyncTest {
    private fun e(id: String, kind: TranscriptKind = TranscriptKind.TRANSCRIPT_KIND_ASSISTANT, text: String = id): TranscriptEntry = TranscriptEntry.newBuilder().setId(id).setKind(kind).setText(text).build()

    private fun page(entries: List<TranscriptEntry>, session: String = "s1", start: Long = 0, end: Long = 100, atStart: Boolean = false): ReadTranscriptResponse =
        ReadTranscriptResponse.newBuilder().setSupported(true).setSessionId(session).addAllEntries(entries).setStartOffset(start).setEndOffset(end).setAtStart(atStart).build()

    @Test fun `the first read is the tail, then it reads on from where it ends`() = runTest {
        val api = RecordingApi().apply { transcript = { _, off, back -> if (back) page(listOf(e("a")), start = 50, end = 100) else page(listOf(e("b")), start = off, end = 160) } }
        val sync = ChatSync(backgroundScope, { api }, "w1")
        sync.sync(); runCurrent()
        assertEquals(listOf(Triple("w1", -1L, true)), api.reads.toList())
        assertEquals(listOf("a"), sync.state.value.transcript.entries.map { it.id })
        assertTrue(sync.state.value.loaded)
        sync.sync(); runCurrent()
        assertEquals(Triple("w1", 100L, false), api.reads.last())
        assertEquals(listOf("a", "b"), sync.state.value.transcript.entries.map { it.id })
    }

    @Test fun `a read asked for while one is out follows it, and not in parallel`() = runTest {
        val gate = CompletableDeferred<Unit>()
        val api = RecordingApi().apply {
            transcript = { _, _, _ -> page(listOf(e("a")), start = 0, end = 100) }
        }
        val slow = object : dev.agentmesh.android.net.MeshApi by api {
            override suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): ReadTranscriptResponse {
                if (api.reads.isEmpty()) gate.await()
                return api.readTranscript(agent, offset, backward)
            }
        }
        val sync = ChatSync(backgroundScope, { slow }, "w1")
        sync.sync(); runCurrent()
        sync.sync(); sync.sync(); runCurrent()
        assertTrue(api.reads.isEmpty())
        gate.complete(Unit); runCurrent()
        // The tail, then one more read for everything asked for meanwhile.
        assertEquals(2, api.reads.size)
    }

    @Test fun `a record that is another conversation now is read again from its tail`() = runTest {
        var session = "s1"
        val api = RecordingApi().apply { transcript = { _, _, _ -> page(listOf(e("x-$session")), session = session, start = 0, end = 100) } }
        val sync = ChatSync(backgroundScope, { api }, "w1")
        sync.sync(); runCurrent()
        session = "s2"
        sync.sync(); runCurrent()
        assertEquals(listOf("x-s2"), sync.state.value.transcript.entries.map { it.id })
        assertEquals(Triple("w1", -1L, true), api.reads.last())
    }

    @Test fun `earlier entries come in above, and a page that doesn't join is dropped for a fresh tail`() = runTest {
        val api = RecordingApi().apply {
            transcript = { _, off, back -> if (back && off == -1L) page(listOf(e("b")), start = 50, end = 100) else page(listOf(e("z")), start = 0, end = 50, atStart = true) }
        }
        val sync = ChatSync(backgroundScope, { api }, "w1")
        sync.sync(); runCurrent()
        sync.loadEarlier(); runCurrent()
        assertEquals(listOf("z", "b"), sync.state.value.transcript.entries.map { it.id })
        assertTrue(sync.state.value.transcript.atStart)
        assertFalse(sync.state.value.earlierBusy)
        // A page that ends elsewhere than where the held entries begin: the tail is read again.
        val api2 = RecordingApi().apply {
            transcript = { _, off, back -> if (back && off == -1L) page(listOf(e("b")), start = 50, end = 100) else page(listOf(e("z")), start = 0, end = 20) }
        }
        val sync2 = ChatSync(backgroundScope, { api2 }, "w2")
        sync2.sync(); runCurrent()
        sync2.loadEarlier(); runCurrent()
        assertEquals(listOf("b"), sync2.state.value.transcript.entries.map { it.id })
        assertEquals(3, api2.reads.size)
    }

    @Test fun `a failed read shows the host's words, and the next one clears them`() = runTest {
        val api = RecordingApi().apply { failRead = dev.agentmesh.android.net.ApiException(com.connectrpc.Code.UNAVAILABLE, "[unavailable] down") }
        val sync = ChatSync(backgroundScope, { api }, "w1")
        sync.sync(); runCurrent()
        assertEquals("down", sync.state.value.error)
        api.failRead = null
        api.transcript = { _, _, _ -> page(listOf(e("a"))) }
        sync.sync(); runCurrent()
        assertEquals("", sync.state.value.error)
        assertTrue(sync.state.value.loaded)
    }

    @Test fun `a reset drops what an old read brings`() = runTest {
        val gate = CompletableDeferred<Unit>()
        val api = RecordingApi().apply { transcript = { _, _, _ -> page(listOf(e("old"))) } }
        val slow = object : dev.agentmesh.android.net.MeshApi by api {
            override suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): ReadTranscriptResponse {
                if (api.reads.isEmpty()) gate.await()
                return api.readTranscript(agent, offset, backward)
            }
        }
        val sync = ChatSync(backgroundScope, { slow }, "w1")
        sync.sync(); runCurrent()
        sync.reset()
        gate.complete(Unit); runCurrent()
        assertFalse(sync.state.value.loaded)
        assertTrue(sync.state.value.transcript.entries.isEmpty())
    }

    @Test fun `after a prompt it reads again at 300 ms and 1500 ms`() = runTest {
        val api = RecordingApi().apply { transcript = { _, _, _ -> page(listOf(e("a"))) } }
        val sync = ChatSync(backgroundScope, { api }, "w1")
        sync.sync(); runCurrent()
        val before = api.reads.size
        sync.afterSend()
        advanceTimeBy(310); runCurrent()
        assertEquals(before + 1, api.reads.size)
        advanceTimeBy(1_300); runCurrent()
        assertEquals(before + 2, api.reads.size)
    }

    @Test fun `after a stop mid-read, the same sync reads again`() = runTest {
        val gate = CompletableDeferred<Unit>()
        val api = RecordingApi().apply { transcript = { _, _, _ -> page(listOf(e("a"))) } }
        val slow = object : dev.agentmesh.android.net.MeshApi by api {
            override suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): ReadTranscriptResponse {
                if (api.reads.isEmpty()) gate.await()
                return api.readTranscript(agent, offset, backward)
            }
        }
        val sync = ChatSync(backgroundScope, { slow }, "w1")
        sync.sync(); runCurrent()
        sync.stop()
        gate.complete(Unit); runCurrent()
        sync.sync(); runCurrent()
        assertTrue(sync.state.value.loaded)
    }

    @Test fun `a Load earlier still out when the record is rewritten is dropped, and stop clears its busy flag`() = runTest {
        val gate = CompletableDeferred<Unit>()
        var session = "s1"
        val api = RecordingApi().apply {
            transcript = { _, off, back -> if (back && off != -1L) page(listOf(e("z")), start = 0, end = 50, atStart = true) else page(listOf(e("x-$session")), session = session, start = 50, end = 100) }
        }
        val slow = object : dev.agentmesh.android.net.MeshApi by api {
            override suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): ReadTranscriptResponse {
                if (backward && offset != -1L) gate.await()
                return api.readTranscript(agent, offset, backward)
            }
        }
        val sync = ChatSync(backgroundScope, { slow }, "w1")
        sync.sync(); runCurrent()
        sync.loadEarlier(); runCurrent()
        assertTrue(sync.state.value.earlierBusy)
        // The record is another conversation now: the view starts over on its tail.
        session = "s2"
        sync.sync(); runCurrent()
        assertEquals(listOf("x-s2"), sync.state.value.transcript.entries.map { it.id })
        // The page of the old conversation arrives late and changes nothing.
        gate.complete(Unit); runCurrent()
        assertEquals(listOf("x-s2"), sync.state.value.transcript.entries.map { it.id })
        assertFalse(sync.state.value.earlierBusy)

        // And a stop while one is out doesn't leave the flag set.
        val gate2 = CompletableDeferred<Unit>()
        val slow2 = object : dev.agentmesh.android.net.MeshApi by api {
            override suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): ReadTranscriptResponse {
                if (backward && offset != -1L) gate2.await()
                return api.readTranscript(agent, offset, backward)
            }
        }
        val sync2 = ChatSync(backgroundScope, { slow2 }, "w2")
        sync2.sync(); runCurrent()
        sync2.loadEarlier(); runCurrent()
        sync2.stop()
        assertFalse(sync2.state.value.earlierBusy)
    }
}
