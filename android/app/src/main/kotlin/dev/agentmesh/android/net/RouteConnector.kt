package dev.agentmesh.android.net

import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.OkHttpClient
import java.security.SecureRandom
import java.time.Duration
import javax.net.ssl.HostnameVerifier
import javax.net.ssl.SSLContext

/** A route that can't be used, and why: shown to the user as it is. */
class RouteException(message: String) : Exception(message)

/** The address and the HTTP client for one route. */
class Connection(val baseUrl: String, val http: OkHttpClient)

/**
 * Turns a [Route] into a connection: a direct address, or the relay's raw TCP endpoint. Every request carries
 * `Authorization: Bearer <token>`, the token read when the request is made, and never an `Origin` header (meshd
 * refuses those).
 */
fun interface RouteConnector {
    fun connect(route: Route, token: () -> String): Connection
}

class DefaultRouteConnector(private val base: OkHttpClient = OkHttpClient()) : RouteConnector {
    override fun connect(route: Route, token: () -> String): Connection = when (route) {
        is Route.Direct -> direct(route, token)
        is Route.Relay -> relay(route, token)
    }

    /** What both routes share: timeouts, the bearer token (when there is one), and the pin's trust. */
    private fun client(token: (() -> String)?, pin: String?): OkHttpClient.Builder {
        val builder = base.newBuilder()
            .connectTimeout(Duration.ofSeconds(10))
            // A stream has no read timeout: the heartbeat (HostConnection) is what notices a dead connection.
            .readTimeout(Duration.ZERO)
            .writeTimeout(Duration.ofSeconds(10))
            .pingInterval(Duration.ofSeconds(15))
        if (token != null) builder.addInterceptor { chain ->
            chain.proceed(chain.request().newBuilder().header("Authorization", "Bearer ${token()}").build())
        }
        if (pin != null) {
            val trust = PinnedTrust(pin)
            val ctx = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trust), SecureRandom()) }
            // The pin names the daemon's certificate; its name (often a self-signed one) isn't what identifies it.
            builder.sslSocketFactory(ctx.socketFactory, trust).hostnameVerifier(HostnameVerifier { _, _ -> true })
        }
        return builder
    }

    /**
     * The relay's raw TCP endpoint: every connection starts with the `MESH1` preamble ([RelaySocketFactory]) and then
     * speaks TLS pinned to the host's certificate, so the relay sees only ciphertext. No proxy, no system trust and no
     * name check: the pin is the only way in.
     */
    private fun relay(route: Route.Relay, token: (() -> String)?): Connection {
        if (splitHostPort(route.relayTcp) == null) throw RouteException("the relay address \"${route.relayTcp}\" isn't host:port")
        if (route.hostId.isBlank()) throw RouteException("this route has no host id at the relay")
        if (!isPreambleWord(route.hostId) || (route.relayClientToken.isNotBlank() && !isPreambleWord(route.relayClientToken))) throw RouteException("this route's host id or relay token has characters the relay can't take: pair with the host again")
        if (route.relayClientToken.isBlank()) throw RouteException("this device can't read the saved relay token: pair with the host again")
        if (!Fingerprint.isValid(route.pinSha256)) throw RouteException("the pinned fingerprint isn't a SHA-256 (64 hex digits)")
        val http = client(token, route.pinSha256)
            .proxy(java.net.Proxy.NO_PROXY).dns(RelayDns)
            .socketFactory(RelaySocketFactory(route.relayTcp, route.hostId, { route.relayClientToken }))
            .build()
        return Connection("https://$RELAY_PLACEHOLDER_HOST", http)
    }

    /** A connection that sends no `Authorization`, for the one call a device makes before it has a token: redeeming a pairing code. */
    fun anonymous(route: Route): Connection = when (route) {
        is Route.Relay -> relay(route, null)
        is Route.Direct -> direct(route, null)
    }

    private fun direct(route: Route.Direct, token: (() -> String)?): Connection {
        val url = route.url.trim().trimEnd('/').toHttpUrlOrNull() ?: throw RouteException("\"${route.url}\" is not a URL")
        if (url.scheme == "http" && url.host !in CLEARTEXT_HOSTS) {
            throw RouteException("http:// goes only to this device or the emulator's host: use https:// (for example tailscale serve) for ${url.host}")
        }
        val pin = route.pinSha256?.takeIf { it.isNotBlank() }
        if (pin != null) {
            if (url.scheme != "https") throw RouteException("a certificate pin needs an https:// address")
            if (!Fingerprint.isValid(pin)) throw RouteException("the pinned fingerprint isn't a SHA-256 (64 hex digits)")
        }
        return Connection(url.toString().trimEnd('/'), client(token, pin).build())
    }

    companion object {
        /** Plain HTTP only where nothing is on the wire: this device, and the Android emulator's view of its host. */
        val CLEARTEXT_HOSTS = setOf("localhost", "127.0.0.1", "10.0.2.2")
    }
}
