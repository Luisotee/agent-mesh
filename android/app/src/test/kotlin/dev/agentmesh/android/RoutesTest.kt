package dev.agentmesh.android

import dev.agentmesh.android.net.DefaultRouteConnector
import dev.agentmesh.android.net.Fingerprint
import dev.agentmesh.android.net.PinnedTrust
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.net.RouteException
import dev.agentmesh.android.net.isTailnetUrl
import okhttp3.Request
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.tls.HandshakeCertificates
import okhttp3.tls.HeldCertificate
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.security.cert.CertificateException
import javax.net.ssl.SSLException

class RoutesTest {
    private val server = MockWebServer()
    @After fun tearDown() { runCatching { server.shutdown() } }

    private fun https(cert: HeldCertificate): String {
        val hc = HandshakeCertificates.Builder().heldCertificate(cert).build()
        server.useHttps(hc.sslSocketFactory(), false)
        server.enqueue(MockResponse().setBody("ok"))
        server.start()
        return "https://localhost:${server.port}"
    }

    private fun get(url: String, pin: String?, token: String = "tok"): String {
        val c = DefaultRouteConnector().connect(Route.Direct(url, pin)) { token }
        return c.http.newCall(Request.Builder().url(c.baseUrl + "/x").build()).execute().use { it.body.string() }
    }

    private fun selfSigned() = HeldCertificate.Builder().commonName("meshd").addSubjectAlternativeName("not-localhost.invalid").build()
    private fun pinOf(c: HeldCertificate) = Fingerprint.of(c.certificate)

    @Test fun `the pinned certificate is accepted even though no authority signed it and its name doesn't match`() {
        val cert = selfSigned()
        assertEquals("ok", get(https(cert), pinOf(cert)))
    }

    @Test fun `any other certificate is refused`() {
        val real = selfSigned()
        val other = selfSigned()
        val e = runCatching { get(https(real), pinOf(other)) }.exceptionOrNull()
        assertTrue("got $e", e is SSLException)
        assertTrue(generateSequence(e) { it.cause }.any { it is CertificateException && "not the pinned one" in it.message.orEmpty() })
    }

    @Test fun `without a pin the system's authorities decide, so a self-signed host is refused`() {
        val e = runCatching { get(https(selfSigned()), null) }.exceptionOrNull()
        assertTrue("got $e", e is SSLException)
    }

    @Test fun `every request carries the token read at call time, and no Origin`() {
        server.enqueue(MockResponse().setBody("a")); server.enqueue(MockResponse().setBody("b")); server.start()
        var token = "first"
        val c = DefaultRouteConnector().connect(Route.Direct("http://127.0.0.1:${server.port}")) { token }
        c.http.newCall(Request.Builder().url(c.baseUrl + "/x").build()).execute().close()
        token = "second"
        c.http.newCall(Request.Builder().url(c.baseUrl + "/x").build()).execute().close()
        val r1 = server.takeRequest(); val r2 = server.takeRequest()
        assertEquals("Bearer first", r1.getHeader("Authorization")); assertEquals("Bearer second", r2.getHeader("Authorization"))
        assertNull(r1.getHeader("Origin")); assertNull(r1.getHeader("Sec-Fetch-Site"))
    }

    @Test fun `plain http goes only to this device and the emulator's host`() {
        val c = DefaultRouteConnector()
        for (ok in listOf("http://localhost:1", "http://127.0.0.1:1", "http://10.0.2.2:8080")) c.connect(Route.Direct(ok)) { "" }
        for (bad in listOf("http://192.168.1.5:1", "http://100.64.0.1:1", "http://example.com")) {
            val e = runCatching { c.connect(Route.Direct(bad)) { "" } }.exceptionOrNull()
            assertTrue("$bad: $e", e is RouteException)
        }
    }

    @Test fun `a pin needs https and a SHA-256, and nonsense is refused`() {
        val c = DefaultRouteConnector()
        assertTrue(runCatching { c.connect(Route.Direct("http://localhost:1", "ab".repeat(32))) { "" } }.exceptionOrNull() is RouteException)
        assertTrue(runCatching { c.connect(Route.Direct("https://h", "abcd")) { "" } }.exceptionOrNull() is RouteException)
        assertTrue(runCatching { c.connect(Route.Direct("not a url")) { "" } }.exceptionOrNull() is RouteException)
    }

    @Test fun `a relay route is built from its address, host, token and pin, and anything missing is a refusal that says what`() {
        val c = DefaultRouteConnector()
        val ok = c.connect(Route.Relay("relay.example:8443", "h1", "amr_x", "ab".repeat(32))) { "" }
        assertEquals("https://mesh-relay.invalid", ok.baseUrl)
        for ((route, word) in listOf(
            Route.Relay("relay.example", "h1", "amr_x", "ab".repeat(32)) to "host:port",
            Route.Relay("relay.example:8443", "", "amr_x", "ab".repeat(32)) to "host id",
            Route.Relay("relay.example:8443", "h1", "", "ab".repeat(32)) to "pair with the host again",
            Route.Relay("relay.example:8443", "h1", "amr_x", "abcd") to "SHA-256",
        )) {
            val e = runCatching { c.connect(route) { "" } }.exceptionOrNull()
            assertTrue("$word: $e", e is RouteException && word in e.message.orEmpty())
        }
    }

    @Test fun `fingerprints compare however they are written`() {
        val hex = "ab".repeat(32)
        assertEquals(hex, Fingerprint.normalize(hex.uppercase().chunked(2).joinToString(":")))
        assertEquals(hex, Fingerprint.normalize("sha256:$hex"))
        assertTrue(Fingerprint.isValid("sha256:$hex"))
        assertFalse(Fingerprint.isValid("ab".repeat(31)))
        assertFalse(PinnedTrust(hex) === null)
    }

    @Test fun `tailnet addresses are told from other direct routes`() {
        for (u in listOf("https://100.64.0.1:9443", "https://100.127.255.254", "https://mesh-home.tail1234.ts.net", "https://MESH.TS.NET:8443", "https://[fd7a:115c:a1e0::7]:443")) assertTrue(u, isTailnetUrl(u))
        for (u in listOf("https://100.63.255.255", "https://100.128.0.1", "https://100.64.0.256", "https://192.168.1.5:9443", "https://example.com", "https://ts.net.evil.com", "not a url", "")) assertFalse(u, isTailnetUrl(u))
    }
}
