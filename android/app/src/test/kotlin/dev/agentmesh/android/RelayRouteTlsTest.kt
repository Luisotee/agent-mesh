package dev.agentmesh.android

import dev.agentmesh.android.net.DefaultRouteConnector
import dev.agentmesh.android.net.Fingerprint
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.net.describeFailure
import dev.agentmesh.android.net.relayRefusal
import okhttp3.Request
import okhttp3.tls.HandshakeCertificates
import okhttp3.tls.HeldCertificate
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.net.InetAddress
import java.net.ServerSocket
import java.net.Socket
import java.util.concurrent.CopyOnWriteArrayList
import javax.net.ssl.SSLSocket
import kotlin.concurrent.thread

/**
 * The relay route end to end without the Go binaries (daemon/e2e's counterpart is `PairingE2ETest`): a stand-in relay
 * that answers the preamble and then splices to a TLS server, and the app's own client in front of it. Proves that
 * OkHttp runs TLS on the socket the factory hands back, pinned, with the bearer token and without an `Origin`.
 */
class RelayRouteTlsTest {
    private val held = HeldCertificate.Builder().commonName("meshd").build()
    private val server = HandshakeCertificates.Builder().heldCertificate(held).build()
    private val relay = ServerSocket(0, 20, InetAddress.getLoopbackAddress())
    private val requests = CopyOnWriteArrayList<String>()
    private val preambles = CopyOnWriteArrayList<String>()
    @Volatile private var answer = "OK"

    @After fun close() { relay.close() }

    private fun Socket.readLine(): String {
        val sb = StringBuilder()
        while (true) { val b = getInputStream().read(); if (b < 0 || b == '\n'.code) return sb.toString(); sb.append(b.toChar()) }
    }

    init {
        thread(isDaemon = true) {
            try {
                while (true) {
                    val raw = relay.accept()
                    thread(isDaemon = true) {
                        runCatching {
                            preambles += raw.readLine()
                            if (answer != "OK") { raw.getOutputStream().write("$answer\n".toByteArray()); raw.close(); return@runCatching }
                            raw.getOutputStream().write("OK\n".toByteArray()); raw.getOutputStream().flush()
                            // From here the relay only splices: the "host" speaks TLS on the same socket.
                            val tls = server.sslSocketFactory().createSocket(raw, null, raw.port, true) as SSLSocket
                            tls.useClientMode = false
                            tls.startHandshake()
                            val input = tls.getInputStream().bufferedReader()
                            while (true) {
                                val head = StringBuilder()
                                while (true) { val l = input.readLine() ?: return@runCatching; if (l.isEmpty()) break; head.appendLine(l) }
                                requests += head.toString()
                                tls.getOutputStream().write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi".toByteArray()); tls.getOutputStream().flush()
                            }
                        }
                    }
                }
            } catch (_: Exception) {}
        }
    }

    private fun route(pin: String = Fingerprint.of(held.certificate), token: String = "amr_relay") =
        Route.Relay("127.0.0.1:${relay.localPort}", "h123", token, pin)

    private fun get(route: Route.Relay, bearer: String = "amt_device") =
        DefaultRouteConnector().connect(route) { bearer }.let { c -> c.http.newCall(Request.Builder().url(c.baseUrl + "/agentmesh.v1.MeshService/Hello").build()).execute().use { it.body.string() } }

    @Test fun `a request through the relay runs pinned TLS on the spliced socket, with the device token and no Origin`() {
        assertEquals("hi", get(route()))
        assertEquals("MESH1 h123 amr_relay", preambles.single())
        val head = requests.single().lowercase()
        assertTrue(head, "authorization: bearer amt_device" in head)
        assertTrue(head, "origin:" !in head && "sec-fetch-site" !in head)
    }

    @Test fun `each new connection is a relay stream of its own, and a later request reuses the open one`() {
        val c = DefaultRouteConnector().connect(route()) { "t" }
        repeat(3) { c.http.newCall(Request.Builder().url(c.baseUrl + "/x").build()).execute().use { it.body.string() } }
        assertEquals("one connection carried the three requests", 1, preambles.size)
        c.http.connectionPool.evictAll()
        c.http.newCall(Request.Builder().url(c.baseUrl + "/x").build()).execute().use { it.body.string() }
        assertEquals(2, preambles.size)
    }

    @Test fun `another certificate than the pinned one is refused, whatever its name`() {
        val other = HeldCertificate.Builder().commonName("mesh-relay.invalid").addSubjectAlternativeName("mesh-relay.invalid").build()
        val e = runCatching { get(route(pin = Fingerprint.of(other.certificate))) }.exceptionOrNull()
        assertNotNull(e)
        assertTrue("got ${describeFailure(e!!)}", "pinned" in describeFailure(e))
        assertTrue("nothing was requested of the host", requests.isEmpty())
    }

    @Test fun `a relay that refuses the stream is reported in its own words`() {
        answer = "ERR auth bad token"
        val e = runCatching { get(route()) }.exceptionOrNull()
        assertEquals("auth", relayRefusal(e!!)?.code)
        assertTrue(describeFailure(e), "refused this device's client token" in describeFailure(e))
        answer = "ERR offline no daemon"
        assertTrue(describeFailure(runCatching { get(route()) }.exceptionOrNull()!!).contains("offline at the relay"))
    }
}
