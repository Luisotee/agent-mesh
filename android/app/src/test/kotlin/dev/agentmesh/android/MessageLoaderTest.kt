package dev.agentmesh.android

import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.store.MessageLoader
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class MessageLoaderTest {
    private fun m(id: String, at: Long, run: String = "r1") = message(id, runId = run, from = "a", to = "b", type = MessageType.MESSAGE_TYPE_INFO, at = at)

    private class Rig(val api: RecordingApi, scope: kotlinx.coroutines.CoroutineScope) {
        var state = HostState()
        val logs = mutableListOf<String>()
        val loader = MessageLoader(scope, { api }, { state }, { f -> state = f(state) }, { logs += it }, retryBaseMs = 1_000)
    }

    @Test fun `it pages through a run's history, oldest first, until there is no more`() = runTest {
        val api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(m("m1", 1), m("m2", 2)), listOf(m("m3", 3)))) }
        val rig = Rig(api, backgroundScope)
        rig.loader.load(listOf(run("r1")))
        runCurrent()
        assertEquals(listOf("", "m2"), api.listCalls.map { it.second })
        assertEquals(setOf("m1", "m2", "m3"), rig.state.messages.keys)
    }

    @Test fun `closed runs are not loaded`() = runTest {
        val api = RecordingApi()
        Rig(api, backgroundScope).loader.load(listOf(run("r1", status = RunStatus.RUN_STATUS_CLOSED)))
        runCurrent()
        assertEquals(emptyList<Pair<String, String>>(), api.listCalls)
    }

    @Test fun `a run that loaded completely resumes after what the listing reached, not after a live message`() = runTest {
        val api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(m("m1", 1), m("m2", 2)))) }
        val rig = Rig(api, backgroundScope)
        rig.loader.load(listOf(run("r1"))); runCurrent()
        assertEquals(setOf("r1"), rig.state.messagesLoaded)
        // Messages sent during a gap are missing, and a live one reached us first: the resync must still fetch the gap.
        rig.state = rig.state.copy(messages = rig.state.messages + ("m9" to m("m9", 9)), messagesLoaded = emptySet())
        api.listCalls.clear()
        rig.loader.load(listOf(run("r1"))); runCurrent()
        assertEquals(listOf("m2"), api.listCalls.map { it.second })
        assertEquals(setOf("r1"), rig.state.messagesLoaded)
    }

    @Test fun `the run is loaded only when its listing finished, and failed when the retries are used up`() = runTest {
        val api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(m("m1", 1)))); failListings = 100 }
        val rig = Rig(api, backgroundScope)
        rig.loader.load(listOf(run("r1"))); runCurrent()
        assertEquals(emptySet<String>(), rig.state.messagesLoaded)
        assertEquals(emptySet<String>(), rig.state.messagesFailed)
        advanceTimeBy(1_000L * (1 + 2 + 4 + 8 + 16) + 100); runCurrent()
        assertEquals(setOf("r1"), rig.state.messagesFailed)
        // Asked again, it clears the verdict and succeeds.
        api.failListings = 0
        rig.loader.load(listOf(run("r1"))); runCurrent()
        assertEquals(setOf("r1"), rig.state.messagesLoaded)
        assertEquals(emptySet<String>(), rig.state.messagesFailed)
    }

    @Test fun `a new snapshot replaces the retry that was waiting, so retries don't multiply`() = runTest {
        val api = RecordingApi().apply { failListings = 100 }
        val rig = Rig(api, backgroundScope)
        // A flapping connection: a snapshot every half second, each load failing.
        repeat(20) { rig.loader.load(listOf(run("r1"))); runCurrent(); advanceTimeBy(500) }
        advanceTimeBy(60_000); runCurrent()
        // Each snapshot makes one attempt, and the last one's retries run out: nothing was doubled up.
        assertEquals(20 + 5, api.listCalls.size)
    }

    @Test fun `reset forgets what a run had loaded and stops the waiting retries`() = runTest {
        val api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(m("m1", 1), m("m2", 2)), listOf(m("m3", 3)))) }
        val rig = Rig(api, backgroundScope)
        rig.loader.load(listOf(run("r1"))); runCurrent()
        rig.loader.reset()
        api.listCalls.clear()
        rig.loader.load(listOf(run("r1"))); runCurrent()
        assertEquals("starts over from the first page", listOf("", "m2"), api.listCalls.map { it.second })
        api.failListings = 100
        rig.loader.load(listOf(run("r1"))); runCurrent()
        val n = api.listCalls.size
        rig.loader.reset()
        advanceTimeBy(120_000); runCurrent()
        assertEquals(n, api.listCalls.size)
    }

    @Test fun `a failed load is tried again with a growing wait, then given up`() = runTest {
        val api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(m("m1", 1)))); failListings = 2 }
        val rig = Rig(api, backgroundScope)
        rig.loader.load(listOf(run("r1"))); runCurrent()
        assertEquals(1, api.listCalls.size)
        advanceTimeBy(1_001); runCurrent()
        assertEquals(2, api.listCalls.size)
        advanceTimeBy(2_001); runCurrent()
        assertEquals("the third attempt succeeds", setOf("m1"), rig.state.messages.keys)
        assertEquals(2, rig.logs.size)

        // A later snapshot starts the retries over: it isn't left without any after an earlier run of failures.
        val flaky = RecordingApi().apply { failListings = 100 }
        val rigF = Rig(flaky, backgroundScope)
        rigF.loader.load(listOf(run("r1"))); advanceTimeBy(1_000L * (1 + 2 + 4 + 8 + 16) + 100); runCurrent()
        val tries = flaky.listCalls.size
        flaky.failListings = 1
        rigF.loader.load(listOf(run("r1"))); runCurrent(); advanceTimeBy(1_100); runCurrent()
        assertTrue("retried after the new request", flaky.listCalls.size >= tries + 2)

        val dead = RecordingApi().apply { failListings = 100 }
        val rig2 = Rig(dead, backgroundScope)
        rig2.loader.load(listOf(run("r1"))); advanceTimeBy(1_000L * (1 + 2 + 4 + 8 + 16) + 100); runCurrent()
        assertEquals("one try and five retries", 6, dead.listCalls.size)
        advanceTimeBy(120_000); runCurrent()
        assertEquals(6, dead.listCalls.size)
    }
}
