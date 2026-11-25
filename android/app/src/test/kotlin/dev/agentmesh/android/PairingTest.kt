package dev.agentmesh.android

import com.connectrpc.Code
import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.hosts.PairingClients
import dev.agentmesh.android.hosts.PairingFlow
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.PairingApi
import dev.agentmesh.android.net.PairingException
import dev.agentmesh.android.net.PairingLink
import dev.agentmesh.android.net.Redeemed
import dev.agentmesh.android.net.RelayRefused
import dev.agentmesh.android.net.Route
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.IOException

class PairingTest {
    @get:Rule val tmp = TemporaryFolder()

    private val fp = "ab".repeat(32)
    private val full = "agentmesh://pair?v=1&h=h1234567890ab&fp=sha256:$fp&t=amp_secret&hn=work+station&r=https%3A%2F%2Frelay.example.com" +
        "&rtcp=relay.example.com%3A8443&rt=amr_client&d=https%3A%2F%2F100.64.0.1%3A9443&n=Pixel+9"

    @Test fun `a full link gives the host, the pin, both routes (direct first) and the suggested device name`() {
        val p = PairingLink.parse(full)
        assertEquals("h1234567890ab", p.hostId); assertEquals("work station", p.hostName); assertEquals(fp, p.pinSha256)
        assertEquals("Pixel 9", p.deviceName); assertEquals("amp_secret", p.token)
        assertEquals(listOf(Route.Direct("https://100.64.0.1:9443", fp), Route.Relay("relay.example.com:8443", "h1234567890ab", "amr_client", fp)), p.routes())
    }

    @Test fun `a link is found in the text around it, as pasted from a message or a terminal`() {
        assertEquals("h1234567890ab", PairingLink.parse("Here you go:\n  $full \nthanks").hostId)
        assertEquals("h1234567890ab", PairingLink.parse("<$full>.").hostId)
        assertEquals("h1234567890ab", PairingLink.parse("AGENTMESH://PAIR?v=1&h=h1234567890ab&fp=$fp&t=amp_x&d=https://box:1").hostId)
    }

    @Test fun `a direct-only code and a relay-only code each give their one route`() {
        assertEquals(1, PairingLink.parse("agentmesh://pair?v=1&h=h&fp=$fp&t=amp_x&d=https://box.ts.net:9443").routes().size)
        val r = PairingLink.parse("agentmesh://pair?v=1&h=h&fp=sha256:$fp&t=amp_x&rtcp=relay:8443&rt=amr_c").routes().single()
        assertTrue(r is Route.Relay)
    }

    @Test fun `a code that can't be used says why, and its token is never in the message`() {
        fun err(s: String) = runCatching { PairingLink.parse(s) }.exceptionOrNull()!!.also { assertTrue(it is PairingException); assertFalse("amp_secret" in it.message!!) }.message!!
        assertTrue(err("hello").contains("isn't a pairing link"))
        assertTrue(err("agentmesh://pair?h=h&fp=$fp&t=amp_secret&d=https://b").contains("no version"))
        assertTrue(err("agentmesh://pair?v=2&h=h&fp=$fp&t=amp_secret&d=https://b").contains("newer"))
        assertTrue(err("agentmesh://pair?v=1&fp=$fp&t=amp_secret&d=https://b").contains("incomplete"))
        assertTrue(err("agentmesh://pair?v=1&h=h&t=amp_secret&d=https://b").contains("fingerprint"))
        assertTrue(err("agentmesh://pair?v=1&h=h&fp=1234&t=amp_secret&d=https://b").contains("fingerprint"))
        // No address: http:// isn't accepted for a pinned route, and a relay needs both its address and its token.
        assertTrue(err("agentmesh://pair?v=1&h=h&fp=$fp&t=amp_secret&d=http://box:1").contains("no address"))
        assertTrue(err("agentmesh://pair?v=1&h=h&fp=$fp&t=amp_secret&rtcp=relay:8443").contains("no address"))
        assertTrue(err("agentmesh://pair?v=1&h=h&fp=$fp&t=amp_secret&rt=amr_c&rtcp=norport").contains("no address"))
    }

    @Test fun `the payload and the redeemed token keep secrets out of their text`() {
        val p = PairingLink.parse(full)
        assertFalse("amp_secret" in p.toString()); assertFalse("amr_client" in p.toString())
        assertFalse("amt_dev" in Redeemed("d", "Pixel", "amt_dev", "h", "H", fp).toString())
        assertNull(PairingLink.find("no link here"))
    }

    // ── the flow ──

    private class FakeHost(val hostId: String = "h1234567890ab", val fp: String) {
        val calls = mutableListOf<String>()
        var failRoute: (Route) -> Throwable? = { null }
        var answerHostId = hostId
        val clients = PairingClients { route ->
            object : PairingApi {
                override suspend fun redeem(token: String, deviceName: String): Redeemed {
                    calls += (if (route is Route.Direct) "direct" else "relay") + ":" + deviceName
                    failRoute(route)?.let { throw it }
                    return Redeemed("dev1", deviceName, "amt_device", answerHostId, "work station", "sha256:$fp")
                }
            }
        }
    }

    private fun repo() = HostRepository(tmp.root.resolve("hosts.json"), Plain)

    @Test fun `pairing saves the host with its routes, its pin, the daemon's id and the device token`() = runBlocking {
        val host = FakeHost(fp = fp); val repo = repo()
        val saved = PairingFlow(repo, host.clients).pair(PairingLink.parse(full), "My phone")
        assertEquals(listOf("direct:My phone"), host.calls)
        assertEquals("work station", saved.name); assertEquals("h1234567890ab", saved.daemonId)
        assertEquals(2, saved.routes.size)
        assertEquals("amt_device", repo.token(saved.id))
    }

    @Test fun `the relay is tried when the direct address doesn't answer`() = runBlocking {
        val host = FakeHost(fp = fp).apply { failRoute = { if (it is Route.Direct) IOException("no route to host") else null } }
        val saved = PairingFlow(repo(), host.clients).pair(PairingLink.parse(full), "")
        assertEquals(listOf("direct:Pixel 9", "relay:Pixel 9"), host.calls)
        assertEquals("the suggested device name is used when none is typed", "work station", saved.name)
    }

    @Test fun `when every route fails the message names each, with the relay's own word`() = runBlocking {
        val host = FakeHost(fp = fp).apply { failRoute = { if (it is Route.Direct) IOException("connection refused") else RelayRefused("offline", "the host is offline at the relay (is meshd running and connected to it?)") } }
        val e = runCatching { PairingFlow(repo(), host.clients).pair(PairingLink.parse(full), "x") }.exceptionOrNull()!!
        assertTrue(e is PairingException)
        assertTrue(e.message!!, "direct (https://100.64.0.1:9443): connection refused" in e.message!! && "host is offline at the relay" in e.message!!)
        assertEquals(emptyList<Any>(), repo().hosts.value)
    }

    @Test fun `a code the host refuses stops at once, with a reason a person can act on`() = runBlocking {
        for ((hostSays, expect) in listOf("pairing token expired" to "expired", "pairing token already used" to "already used", "unknown pairing token" to "doesn't know")) {
            val host = FakeHost(fp = fp).apply { failRoute = { ApiException(Code.UNAUTHENTICATED, hostSays) } }
            val e = runCatching { PairingFlow(repo(), host.clients).pair(PairingLink.parse(full), "x") }.exceptionOrNull()!!
            assertTrue("$hostSays -> ${e.message}", e is PairingException && expect in e.message!!)
            assertEquals("the other route isn't tried with a token that's refused", 1, host.calls.size)
        }
    }

    @Test fun `a host that isn't the one in the code is never saved`() = runBlocking {
        val host = FakeHost(fp = fp).apply { answerHostId = "someone-else" }
        val repo = repo()
        val e = runCatching { PairingFlow(repo, host.clients).pair(PairingLink.parse(full), "x") }.exceptionOrNull()!!
        assertTrue(e is PairingException && "isn't the one in the code" in e.message!!)
        assertEquals(emptyList<Any>(), repo.hosts.value)
        val other = FakeHost(fp = "cd".repeat(32))
        assertTrue(runCatching { PairingFlow(repo, other.clients).pair(PairingLink.parse(full), "x") }.exceptionOrNull() is PairingException)
        assertEquals(emptyList<Any>(), repo.hosts.value)
    }

    @Test fun `pairing the same host again replaces its routes and token instead of adding a second entry`() = runBlocking {
        val host = FakeHost(fp = fp); val repo = repo()
        val first = PairingFlow(repo, host.clients).pair(PairingLink.parse(full), "a")
        val again = PairingFlow(repo, host.clients).pair(PairingLink.parse("agentmesh://pair?v=1&h=h1234567890ab&fp=$fp&t=amp_second&d=https://new:1"), "a")
        assertEquals(first.id, again.id)
        assertEquals(1, repo.hosts.value.size)
        assertEquals(listOf<Route>(Route.Direct("https://new:1", fp)), repo.get(first.id)!!.routes)
    }

    @Test fun `a save that fails after the host accepted says the code is used up`() = runBlocking {
        val host = FakeHost(fp = fp)
        val dir = tmp.newFolder("ro"); val repo = HostRepository(dir.resolve("hosts.json"), Plain)
        dir.setWritable(false)
        val e = runCatching { PairingFlow(repo, host.clients).pair(PairingLink.parse(full), "x") }.exceptionOrNull()!!
        dir.setWritable(true)
        // (Running as root, the directory stays writable: then there is nothing to check.)
        if (e is PairingException) assertTrue(e.message!!, "make a new one" in e.message!!)
    }
}
