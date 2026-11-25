package dev.agentmesh.android.net

import okhttp3.Dns
import java.io.IOException
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.Socket
import java.net.SocketAddress
import javax.net.SocketFactory

/** The relay answered the preamble with an error, or with nothing a relay says. [code] is the relay's word for it. */
class RelayRefused(val code: String, message: String) : IOException(message)

/**
 * The host name every relay connection is made to. It is never looked up or dialled: [RelayDns] answers for it, and
 * [RelaySocketFactory] connects to the relay instead. It is also what the TLS handshake names (the pin, not the
 * name, identifies the host).
 */
const val RELAY_PLACEHOLDER_HOST = "mesh-relay.invalid"

/** Any name resolves to loopback: the relay's own address is the one the socket dials. */
object RelayDns : Dns {
    override fun lookup(hostname: String): List<InetAddress> = listOf(InetAddress.getByAddress(hostname, byteArrayOf(127, 0, 0, 1)))
}

/**
 * The relay's raw TCP client endpoint as an OkHttp [SocketFactory] (docs/relay.md, "Relay raw TCP client
 * listener"): a socket from here connects to [relayTcp] (`host:port`) wherever it is asked to connect, sends
 * `MESH1 <hostId> <token>\n`, waits for `OK\n`, and only then is handed back, so OkHttp's TLS handshake runs on the
 * spliced stream to the host's own listener, end to end and pinned. Every connection OkHttp opens, a terminal's
 * WebSocket included, is a stream of its own at the relay and so goes through the preamble of its own.
 */
class RelaySocketFactory(
    private val relayTcp: String,
    private val hostId: String,
    private val token: () -> String,
    /** How long the relay may take to splice the stream (it dials the host back first). */
    private val answerTimeoutMs: Int = 15_000,
) : SocketFactory() {
    override fun createSocket(): Socket = RelaySocket(relayTcp, hostId, token, answerTimeoutMs)

    override fun createSocket(host: String, port: Int): Socket = createSocket().also { it.connect(InetSocketAddress(host, port)) }
    override fun createSocket(host: String, port: Int, localHost: InetAddress, localPort: Int): Socket = createSocket(host, port)
    override fun createSocket(host: InetAddress, port: Int): Socket = createSocket().also { it.connect(InetSocketAddress(host, port)) }
    override fun createSocket(address: InetAddress, port: Int, localAddress: InetAddress, localPort: Int): Socket = createSocket(address, port)
}

private class RelaySocket(
    private val relayTcp: String,
    private val hostId: String,
    private val token: () -> String,
    private val answerTimeoutMs: Int,
) : Socket() {
    override fun connect(endpoint: SocketAddress, timeout: Int) {
        val (host, port) = splitHostPort(relayTcp) ?: throw IOException("the relay address \"$relayTcp\" isn't host:port")
        try {
            super.connect(InetSocketAddress(host, port), timeout)
            tcpNoDelay = true
            soTimeout = answerTimeoutMs
            getOutputStream().apply { write("MESH1 $hostId ${token()}\n".toByteArray(Charsets.US_ASCII)); flush() }
            val answer = readLine()
            if (answer != "OK") throw refused(answer)
            soTimeout = 0
        } catch (e: IOException) {
            runCatching { close() }
            throw e
        }
    }

    /** One line, byte by byte: whatever follows `OK\n` is the TLS handshake, which must reach the TLS layer untouched. */
    private fun readLine(): String {
        val input = getInputStream()
        val sb = StringBuilder()
        while (true) {
            val b = input.read()
            if (b < 0) throw IOException("the relay closed the connection without answering")
            if (b == '\n'.code) return sb.toString().trimEnd('\r')
            if (sb.length >= MAX_LINE) throw IOException("the relay's answer is not one short line")
            sb.append(b.toChar())
        }
    }

    private fun refused(answer: String): RelayRefused {
        // `ERR <code> <message>`
        val parts = answer.split(' ', limit = 3)
        if (parts.firstOrNull() != "ERR" || parts.size < 2) return RelayRefused("protocol", "the relay answered something else than OK: \"${answer.take(60)}\"")
        val code = parts[1]
        val detail = parts.getOrNull(2).orEmpty()
        val say = when (code) {
            "auth" -> "the relay refused this device's client token"
            "offline" -> "the host is offline at the relay (is meshd running and connected to it?)"
            "busy" -> "the relay has too many connections to this host waiting"
            "timeout" -> "the host didn't answer the relay in time"
            "bad_request" -> "the relay couldn't read the request"
            else -> "the relay refused the connection ($code)"
        }
        return RelayRefused(code, if (detail.isEmpty()) say else "$say: $detail")
    }

    private companion object { const val MAX_LINE = 512 }
}

/** `host:port` (the host may be a bracketed IPv6 address); null when it isn't one. */
fun splitHostPort(s: String): Pair<String, Int>? {
    val t = s.trim()
    val i = t.lastIndexOf(':')
    if (i <= 0 || i == t.length - 1) return null
    val host = t.substring(0, i).removePrefix("[").removeSuffix("]")
    val port = t.substring(i + 1).toIntOrNull() ?: return null
    return if (host.isBlank() || port !in 1..65535) null else host to port
}
