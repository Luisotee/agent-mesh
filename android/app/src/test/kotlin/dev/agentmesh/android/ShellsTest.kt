package dev.agentmesh.android

import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.store.applyFrame
import dev.agentmesh.android.store.applyShell
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.Shell
import dev.agentmesh.gen.agentmesh.v1.ShellChange
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runBlocking
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/** The host's project shells in the app's state: listed after a snapshot, kept by `ShellChange` frames (the desktop's `shells.ts`). */
class ShellsTest {
    @get:Rule val tmp = TemporaryFolder()
    private val started = mutableListOf<HostRuntime>()
    @After fun stop() { started.forEach { it.stop() } }

    private fun shell(id: String, repo: String = "/home/u/shop", exited: Boolean = false) = Shell.newBuilder().setId(id).setRepo(repo).setExited(exited).build()
    private fun change(s: Shell, closed: Boolean = false) = ShellChange.newBuilder().setShell(s).setClosed(closed).build()
    private fun frame(c: ShellChange) = SubscribeEventsResponse.newBuilder().setShell(c).build()

    @Test fun `a shell frame before the list was read only counts, so the read asks again`() {
        val s = applyFrame(HostState(), frame(change(shell("a"))))
        assertNull(s.shells)
        assertEquals(1, s.shellTouch)
    }

    @Test fun `an opened shell joins the list once, an ended one stays marked exited, and one the host never listed is ignored`() {
        var s = HostState(shells = listOf(shell("a")))
        s = applyFrame(s, frame(change(shell("b"))))
        s = applyFrame(s, frame(change(shell("b"))))
        assertEquals(listOf("a", "b"), s.shells!!.map { it.id })
        s = applyFrame(s, frame(change(shell("a"), closed = true)))
        assertEquals(listOf(true, false), s.shells!!.map { it.exited })
        s = applyFrame(s, frame(change(shell("zzz"), closed = true)))
        assertEquals(listOf("a", "b"), s.shells!!.map { it.id })
        assertEquals(4, s.shellTouch)
    }

    private fun runtime(api: RecordingApi): HostRuntime {
        val repo = HostRepository(tmp.newFile("hosts.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = HostRuntime(host.id, repo, { _, _ -> api }, CoroutineScope(Dispatchers.Unconfined))
        rt.start(); started += rt
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        return rt
    }

    @Test fun `the shells are listed when a snapshot arrives, and opening and ending one shows at once`() = runBlocking {
        val api = RecordingApi().apply { shells = listOf(shell("a")) }
        val rt = runtime(api)
        rt.onFrame(snapshot(emptyList(), emptyList()))
        assertEquals(listOf("a"), rt.state.value.shells!!.map { it.id })
        val b = rt.openShell("/home/u/shop")
        assertEquals(listOf("/home/u/shop"), api.opened)
        assertEquals(listOf("a", b.id), rt.state.value.shells!!.map { it.id })
        // The frame that follows is the same shell, not a second one.
        rt.onFrame(frame(change(b)))
        assertEquals(2, rt.state.value.shells!!.size)
        rt.closeShell("a")
        assertEquals(listOf(b.id), rt.state.value.shells!!.map { it.id })
        assertEquals(listOf("a"), api.closed)
    }

    @Test fun `a frame that arrives while the list is out makes the read ask again`() = runBlocking {
        val api = RecordingApi().apply { shells = listOf(shell("a")) }
        lateinit var rt: HostRuntime
        var calls = 0
        val wrapped = object : dev.agentmesh.android.net.MeshApi by api {
            override suspend fun listShells(): List<Shell> {
                val list = api.listShells()
                // The host opens b right after it answered: its frame comes before the answer is applied.
                if (calls++ == 0) { api.shells = api.shells + shell("b"); rt.onFrame(frame(change(shell("b")))) }
                return list
            }
        }
        val repo = HostRepository(tmp.newFile("hosts2.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        rt = HostRuntime(host.id, repo, { _, _ -> wrapped }, CoroutineScope(Dispatchers.Unconfined))
        rt.start(); started += rt
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.refreshShells()
        assertEquals(2, calls)
        assertEquals(listOf("a", "b"), rt.state.value.shells!!.map { it.id })
    }

    @Test fun `a failing list leaves the shells as they were, and says so in the log`() = runBlocking {
        val api = RecordingApi()
        val failing = object : dev.agentmesh.android.net.MeshApi by api { override suspend fun listShells(): List<Shell> = error("down") }
        val repo = HostRepository(tmp.newFile("hosts3.json").also { it.delete() }, Plain)
        val host = repo.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = HostRuntime(host.id, repo, { _, _ -> failing }, CoroutineScope(Dispatchers.Unconfined))
        rt.start(); started += rt
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.refreshShells()
        assertNull(rt.state.value.shells)
        assertTrue(rt.state.value.log.any { "shells" in it.text })
    }
}
