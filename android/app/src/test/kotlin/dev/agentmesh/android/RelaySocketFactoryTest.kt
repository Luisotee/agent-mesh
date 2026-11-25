package dev.agentmesh.android

import dev.agentmesh.android.net.RelayDns
import dev.agentmesh.android.net.RelayRefused
import dev.agentmesh.android.net.RelaySocketFactory
import dev.agentmesh.android.net.splitHostPort
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.IOException
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread

/** The relay's raw TCP client endpoint, played by a thread: what the app writes, and what it does with each answer. */
class RelaySocketFactoryTest {
    private val server = ServerSocket(0, 50, java.net.InetAddress.getLoopbackAddress())
    private val addr get() = "127.0.0.1:${server.localPort}"
    private val opened = mutableListOf<Socket>()

    @After fun close() { server.close(); opened.forEach { runCatching { it.close() } } }

    private fun relay(handler: (Socket) -> Unit) = thread(isDaemon = true) {
        try { while (true) { val s = server.accept(); opened += s; thread(isDaemon = true) { runCatching { handler(s) } } } } catch (_: IOException) {}
    }

    private fun Socket.readLine(): String {
        val sb = StringBuilder()
        while (true) { val b = getInputStream().read(); if (b < 0 || b == '\n'.code) return sb.toString(); sb.append(b.toChar()) }
    }

    private fun connect(token: String = "amr_secret", host: String = "h123", timeout: Int = 5_000): Socket {
        val f = RelaySocketFactory(addr, host, { token }, answerTimeoutMs = 2_000)
        // The address OkHttp asks for is a placeholder: the factory dials the relay wherever it is told to connect.
        return f.createSocket().also { it.connect(InetSocketAddress("203.0.113.1", 443), timeout) }
    }

    @Test fun `it sends the preamble, waits for OK, and hands back the stream untouched`() {
        var line = ""
        val sent = CountDownLatch(1)
        relay { s ->
            line = s.readLine()
            // OK and the first bytes of the host's TLS in one write: nothing of it may be eaten by the preamble reader.
            s.getOutputStream().write("OK\nHELLO-TLS".toByteArray()); s.getOutputStream().flush()
            sent.countDown()
            s.getInputStream().read()
        }
        val s = connect()
        assertTrue(sent.await(5, TimeUnit.SECONDS))
        assertEquals("MESH1 h123 amr_secret", line)
        assertTrue(s.tcpNoDelay)
        assertEquals(0, s.soTimeout)
        val got = ByteArray(9)
        var n = 0
        while (n < 9) n += s.getInputStream().read(got, n, 9 - n)
        assertEquals("HELLO-TLS", String(got))
    }

    @Test fun `an error line is refused with the relay's reason, and the socket is closed`() {
        for ((answer, code, text) in listOf(
            Triple("ERR auth bad token", "auth", "refused this device's client token"),
            Triple("ERR offline no daemon", "offline", "host is offline at the relay"),
            Triple("ERR busy", "busy", "too many connections"),
            Triple("ERR timeout dial back", "timeout", "didn't answer the relay in time"),
            Triple("ERR bad_request x", "bad_request", "couldn't read the request"),
            Triple("ERR weird thing", "weird", "refused the connection (weird)"),
        )) {
            val srv = ServerSocket(0, 5, java.net.InetAddress.getLoopbackAddress())
            thread(isDaemon = true) { runCatching { srv.accept().use { s -> s.readLine(); s.getOutputStream().write("$answer\n".toByteArray()) } } }
            val f = RelaySocketFactory("127.0.0.1:${srv.localPort}", "h", { "t" }, answerTimeoutMs = 2_000)
            val sock = f.createSocket()
            val e = runCatching { sock.connect(InetSocketAddress("203.0.113.1", 443), 2_000) }.exceptionOrNull()
            assertTrue("$answer: got $e", e is RelayRefused)
            assertEquals(code, (e as RelayRefused).code)
            assertTrue("$answer: ${e.message}", e.message!!.contains(text))
            assertTrue(sock.isClosed)
            srv.close()
        }
    }

    @Test fun `a relay that answers something else, closes, or says nothing is a failure, not a hang`() {
        relay { s -> s.readLine(); s.getOutputStream().write("HTTP/1.1 400 Bad Request\n".toByteArray()) }
        val e1 = runCatching { connect() }.exceptionOrNull()
        assertTrue("got $e1", e1 is RelayRefused && e1.code == "protocol")

        val closing = ServerSocket(0, 5, java.net.InetAddress.getLoopbackAddress())
        thread(isDaemon = true) { runCatching { closing.accept().use { it.readLine() } } }
        val e2 = runCatching { RelaySocketFactory("127.0.0.1:${closing.localPort}", "h", { "t" }, 2_000).createSocket().connect(InetSocketAddress("203.0.113.1", 443), 2_000) }.exceptionOrNull()
        assertTrue("got $e2", e2 is IOException && e2.message!!.contains("closed"))
        closing.close()

        val silent = ServerSocket(0, 5, java.net.InetAddress.getLoopbackAddress())
        thread(isDaemon = true) { runCatching { silent.accept().use { it.readLine(); Thread.sleep(5_000) } } }
        val t0 = System.nanoTime()
        val e3 = runCatching { RelaySocketFactory("127.0.0.1:${silent.localPort}", "h", { "t" }, 500).createSocket().connect(InetSocketAddress("203.0.113.1", 443), 2_000) }.exceptionOrNull()
        assertTrue("got $e3", e3 is java.net.SocketTimeoutException)
        assertTrue("took too long", System.nanoTime() - t0 < 4_000_000_000L)
        silent.close()
    }

    @Test fun `a relay that can't be reached is a connect failure`() {
        val free = ServerSocket(0).use { it.localPort }
        val e = runCatching { RelaySocketFactory("127.0.0.1:$free", "h", { "t" }).createSocket().connect(InetSocketAddress("203.0.113.1", 443), 2_000) }.exceptionOrNull()
        assertTrue("got $e", e is java.net.ConnectException)
    }

    @Test fun `every socket is a relay stream of its own, and any name resolves to the placeholder`() {
        var count = 0
        relay { s -> s.readLine(); synchronized(this) { count++ }; s.getOutputStream().write("OK\n".toByteArray()); s.getInputStream().read() }
        connect(); connect(); connect()
        Thread.sleep(200)
        assertEquals(3, count)
        assertEquals("loopback", if (RelayDns.lookup("mesh-relay.invalid").single().isLoopbackAddress) "loopback" else "other")
    }

    @Test fun `host and port are split, IPv6 included, and nonsense is refused`() {
        assertEquals("relay.example.com" to 8443, splitHostPort("relay.example.com:8443"))
        assertEquals("::1" to 8443, splitHostPort("[::1]:8443"))
        assertNull(splitHostPort("relay.example.com")); assertNull(splitHostPort(":8443")); assertNull(splitHostPort("h:")); assertNull(splitHostPort("h:0")); assertNull(splitHostPort("h:99999"))
    }

    @Test fun `a host id or token that isn't one word is never written to the relay`() {
        var read = ""
        relay { s -> s.soTimeout = 500; read = runCatching { s.readLine() }.getOrDefault("(nothing)") }
        for ((host, token) in listOf("h\nGET /" to "t", "h" to "a b", "h" to "t\r\nMESH1 other x", "" to "t", "h" to "")) {
            val e = runCatching { connect(token = token, host = host) }.exceptionOrNull()
            assertTrue("$host/$token: $e", e is IOException && e.message!!.contains("can't carry"))
        }
        Thread.sleep(700)
        assertTrue("nothing reached the relay beyond a connection, got \"$read\"", read.isEmpty() || read == "(nothing)")
    }
}
