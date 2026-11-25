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
 * Turns a [Route] into a connection. This is the seam for new routes: the relay worker's route plugs in here
 * (`Route.Relay`), and the pairing screen produces routes. Every request carries `Authorization: Bearer <token>`,
 * the token read when the request is made, and never an `Origin` header (meshd refuses those).
 */
fun interface RouteConnector {
    fun connect(route: Route, token: () -> String): Connection
}

class DefaultRouteConnector(private val base: OkHttpClient = OkHttpClient()) : RouteConnector {
    override fun connect(route: Route, token: () -> String): Connection = when (route) {
        is Route.Direct -> direct(route, token)
        is Route.Relay -> throw RouteException("relay routes arrive with pairing: this app can't use one yet")
    }

    private fun direct(route: Route.Direct, token: () -> String): Connection {
        val url = route.url.trim().trimEnd('/').toHttpUrlOrNull() ?: throw RouteException("\"${route.url}\" is not a URL")
        if (url.scheme == "http" && url.host !in CLEARTEXT_HOSTS) {
            throw RouteException("http:// goes only to this device or the emulator's host: use https:// (for example tailscale serve) for ${url.host}")
        }
        val builder = base.newBuilder()
            .connectTimeout(Duration.ofSeconds(10))
            // A stream has no read timeout: the heartbeat (HostConnection) is what notices a dead connection.
            .readTimeout(Duration.ZERO)
            .writeTimeout(Duration.ofSeconds(10))
            .pingInterval(Duration.ofSeconds(15))
            .addInterceptor { chain ->
                chain.proceed(chain.request().newBuilder().header("Authorization", "Bearer ${token()}").build())
            }
        val pin = route.pinSha256?.takeIf { it.isNotBlank() }
        if (pin != null) {
            if (url.scheme != "https") throw RouteException("a certificate pin needs an https:// address")
            if (!Fingerprint.isValid(pin)) throw RouteException("the pinned fingerprint isn't a SHA-256 (64 hex digits)")
            val trust = PinnedTrust(pin)
            val ctx = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trust), SecureRandom()) }
            // The pin names the daemon's certificate; its name (often a self-signed one) isn't what identifies it.
            builder.sslSocketFactory(ctx.socketFactory, trust).hostnameVerifier(HostnameVerifier { _, _ -> true })
        }
        return Connection(url.toString().trimEnd('/'), builder.build())
    }

    companion object {
        /** Plain HTTP only where nothing is on the wire: this device, and the Android emulator's view of its host. */
        val CLEARTEXT_HOSTS = setOf("localhost", "127.0.0.1", "10.0.2.2")
    }
}
