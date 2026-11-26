package dev.agentmesh.android

import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.hosts.PairingFlow
import dev.agentmesh.android.net.ConnectMeshApi
import dev.agentmesh.android.net.DefaultRouteConnector
import dev.agentmesh.android.net.PairingException
import dev.agentmesh.android.net.PairingLink
import dev.agentmesh.android.net.PairingPayload
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.term.TermClient
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.term.TermState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/**
 * The relay route and pairing against the real binaries on loopback: a `mesh-relay`, and a `meshd` connected to it
 * that also serves TLS (`--listen`), as in daemon/e2e. A link comes from `mesh pair`, and everything the app does
 * with it runs the app's own code: parse, redeem over a route, save the host, connect, sync, open a terminal.
 */
class PairingE2ETest {
    @get:Rule val tmp = TemporaryFolder()
    private var daemon: DaemonHarness? = null
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    @Before fun setUp() {
        assumeTrue("no Go to build the daemon with, or SKIP_E2E is set", DaemonHarness.available)
        val d = DaemonHarness.create()
        daemon = d
        d.start(tls = true, relay = d.relay().start())
    }

    @After fun tearDown() { scope.coroutineContext[kotlinx.coroutines.Job]?.cancel(); daemon?.close() }

    private fun repo() = HostRepository(tmp.root.resolve("hosts-${System.nanoTime()}.json"), Plain)
    private fun payload(link: String = daemon!!.pairingLink()) = PairingLink.parse(link)
    private fun viaRelay(p: PairingPayload) = p.copy(directUrl = null)
    private fun viaDirect(p: PairingPayload) = p.copy(relayTcp = null, relayClientToken = null)

    /** The host registers with the relay a moment after it starts: a relay that says "offline" is retried until it doesn't. */
    private suspend fun pairWhenOnline(flow: PairingFlow, p: PairingPayload, name: String = "Test phone") = withTimeout(30_000) {
        while (true) {
            try { return@withTimeout flow.pair(p, name) } catch (e: PairingException) {
                if ("offline" !in e.message.orEmpty()) throw e
                delay(100)
            }
        }
        @Suppress("UNREACHABLE_CODE") error("unreachable")
    }

    private fun runtime(repo: HostRepository, hostId: String) =
        HostRuntime(hostId, repo, { r, t -> ConnectMeshApi(DefaultRouteConnector().connect(r, t)) }, scope, heartbeatMs = 500)

    private suspend fun HostRuntime.until(what: String, cond: (dev.agentmesh.android.store.HostState) -> Boolean) = withTimeout(30_000) {
        while (!cond(state.value)) { delay(50) }
    }.also { check(cond(state.value)) { "timed out: $what" } }

    @Test fun `the link from mesh pair names both routes and the host's pin`() {
        val d = daemon!!
        val p = payload()
        assertEquals(d.daemonId, p.hostId)
        assertEquals(dev.agentmesh.android.net.Fingerprint.normalize(d.certFingerprint), p.pinSha256)
        val routes = p.routes()
        assertEquals("https://${d.listen()}", (routes[0] as Route.Direct).url)
        assertTrue(routes[1] is Route.Relay)
    }

    @Test fun `a code redeemed over the relay saves the host, which connects, syncs and opens a terminal through the relay`() = runBlocking {
        val d = daemon!!; val repo = repo()
        val saved = pairWhenOnline(PairingFlow(repo), viaRelay(payload()))
        assertEquals(d.daemonId, saved.daemonId)
        assertTrue(saved.routes.single() is Route.Relay)
        val rt = runtime(repo, saved.id).also { it.start() }
        try {
            rt.until("Ready and synced over the relay") { it.conn is ConnState.Ready && it.synced }
            assertEquals(d.daemonId, rt.state.value.daemon!!.daemonId)
            // A run and an agent started on the host come through the relay as events.
            val run = d.ctl("start-run", "--title", "via relay").getValue("run").jsonObject.getValue("id").jsonPrimitive.content
            rt.until("the run") { run in it.runs }
            val repoDir = d.repo("shop-api")
            val agent = d.ctl("spawn", "--run-id", run, "--repo", repoDir.path, "--slug", "fix", "--kind", "claude", "--role", "worker", "--model", "haiku")
                .getValue("agent").jsonObject.getValue("id").jsonPrimitive.content
            withTimeout(30_000) { while (d.ctl("agent", "--agent", agent).getValue("agent").jsonObject.getValue("state").jsonPrimitive.content != "AGENT_STATE_IDLE") delay(100) }
            // The terminal is a WebSocket: a second stream through the relay, with the same pinned TLS.
            val dispatcher = Dispatchers.Default.limitedParallelism(1)
            val screen = TermScreen()
            val client = TermClient(CoroutineScope(dispatcher), agent, { rt.awaitApi() }, screen)
            client.start()
            try {
                withTimeout(30_000) { while (client.state.value != TermState.Live || "fake claude" !in withContext(dispatcher) { screen.visibleText() }) delay(50) }
            } finally { client.stop() }
        } finally { rt.stop() }
    }

    @Test fun `a code redeemed over the direct TLS address works the same, pinned to the certificate`() = runBlocking {
        val d = daemon!!; val repo = repo()
        val saved = PairingFlow(repo).pair(viaDirect(payload()), "Test phone")
        assertEquals(listOf<Route>(Route.Direct("https://${d.listen()}", dev.agentmesh.android.net.Fingerprint.normalize(d.certFingerprint))), saved.routes)
        val rt = runtime(repo, saved.id).also { it.start() }
        try {
            rt.until("Ready and synced over TLS") { it.conn is ConnState.Ready && it.synced }
            val run = d.ctl("start-run", "--title", "direct").getValue("run").jsonObject.getValue("id").jsonPrimitive.content
            rt.until("the run") { run in it.runs }
        } finally { rt.stop() }
    }

    @Test fun `a wrong pin is refused before anything is sent, and the code still works with the right one`() = runBlocking {
        val p = viaDirect(payload()); val repo = repo()
        for (bad in listOf(p, viaRelay(payload()))) {
            val wrong = bad.copy(pinSha256 = "00".repeat(32))
            val e = runCatching { pairWhenOnline(PairingFlow(repo), wrong) }.exceptionOrNull()
            assertTrue("got $e", e is PairingException && "pinned" in e.message.orEmpty())
        }
        assertEquals(emptyList<Any>(), repo.hosts.value)
        // The handshake failed before any request: the one-time token wasn't spent.
        assertEquals(1, pairWhenOnline(PairingFlow(repo), p).routes.size)
    }

    @Test fun `a wrong relay client token is refused by the relay, in its words, and a saved host stops asking`() = runBlocking {
        val d = daemon!!; val repo = repo()
        val p = payload()
        val e = runCatching { pairWhenOnline(PairingFlow(repo), viaRelay(p).copy(relayClientToken = "amr_wrong")) }.exceptionOrNull()
        assertTrue("got $e", e is PairingException && "refused this device's client token" in e.message.orEmpty())
        // A host saved with a token the relay doesn't accept (its secret changed) stops with an Auth reason, not a retry loop.
        val host = repo.add("Stale", listOf(Route.Relay((p.routes()[1] as Route.Relay).relayTcp, d.daemonId, "amr_wrong", p.pinSha256)), "amt_whatever")
        val rt = runtime(repo, host.id).also { it.start() }
        try {
            rt.until("Stopped for auth") { (it.conn as? ConnState.Stopped)?.reason == ConnState.Reason.Auth }
            assertTrue((rt.state.value.conn as ConnState.Stopped).message, "client token" in (rt.state.value.conn as ConnState.Stopped).message)
        } finally { rt.stop() }
    }

    @Test fun `a host whose daemon isn't running is offline at the relay, and says so`() = runBlocking {
        val d = daemon!!
        val p = payload()
        // Wait until it registered, then stop it.
        val probe = runCatching { pairWhenOnline(PairingFlow(repo()), viaRelay(payload())) }
        assertTrue(probe.isSuccess)
        d.stopDaemon()
        val e = runCatching { PairingFlow(repo()).pair(viaRelay(p), "x") }.exceptionOrNull()
        assertTrue("got $e", e is PairingException && "offline at the relay" in e.message.orEmpty())
    }

    @Test fun `a code works once, and an expired one says it expired`() = runBlocking {
        val d = daemon!!
        val p = viaDirect(payload())
        PairingFlow(repo()).pair(p, "first")
        val again = runCatching { PairingFlow(repo()).pair(p, "second") }.exceptionOrNull()
        assertTrue("got $again", again is PairingException && "already used" in again.message.orEmpty())

        val short = viaDirect(payload(d.pairingLink("short", "--ttl-seconds", "1")))
        delay(2_200)
        val expired = runCatching { PairingFlow(repo()).pair(short, "late") }.exceptionOrNull()
        assertTrue("got $expired", expired is PairingException && "expired" in expired.message.orEmpty())

        val unknown = runCatching { PairingFlow(repo()).pair(p.copy(token = "amp_nothing"), "x") }.exceptionOrNull()
        assertTrue("got $unknown", unknown is PairingException && "doesn't know" in unknown.message.orEmpty())
    }

    @Test fun `revoking the device on the host ends its connection and it stops with an auth reason`() = runBlocking {
        val d = daemon!!; val repo = repo()
        val saved = PairingFlow(repo).pair(viaDirect(payload()), "to be revoked")
        val rt = runtime(repo, saved.id).also { it.start() }
        try {
            rt.until("Ready") { it.conn is ConnState.Ready && it.synced }
            val devices = d.ctl("devices").getValue("devices").jsonArray
            val id = devices.first { it.jsonObject.getValue("name").jsonPrimitive.content == "to be revoked" }.jsonObject.getValue("id").jsonPrimitive.content
            d.ctl("revoke-device", "--device-id", id)
            rt.until("Stopped for auth") { (it.conn as? ConnState.Stopped)?.reason == ConnState.Reason.Auth }
        } finally { rt.stop() }
    }
}
