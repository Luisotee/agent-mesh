package dev.agentmesh.android.net

import kotlinx.serialization.Serializable
import java.security.MessageDigest
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import javax.net.ssl.X509TrustManager

/**
 * One way to reach a host (docs/apps.md, "Reaching a host"). A host keeps a list of routes and the app tries them
 * in order. [Direct] is an address meshd (or something in front of it, such as `tailscale serve`) answers on;
 * [Relay] is the user's own relay (`mesh-relay`), reached over its raw TCP endpoint (see [RelaySocketFactory]).
 */
@Serializable
sealed interface Route {
    /** `https://…` (system trust, or the certificate whose SHA-256 is [pinSha256]), or `http://` to loopback only. */
    @Serializable
    data class Direct(val url: String, val pinSha256: String? = null) : Route

    /**
     * The relay's raw TCP endpoint ([relayTcp], `host:port`), the host's id there, the relay client token and the
     * daemon certificate's fingerprint, all from the pairing code (docs/relay.md: the app sends
     * `MESH1 <hostId> <token>`, reads `OK`, then runs pinned TLS on the same socket).
     */
    @Serializable
    data class Relay(val relayTcp: String, val hostId: String, val relayClientToken: String, val pinSha256: String) : Route {
        /** The client token lets anyone open a stream to the host at the relay: it stays out of logs and error messages. */
        override fun toString() = "Relay(relayTcp=$relayTcp, hostId=$hostId, relayClientToken=…, pinSha256=$pinSha256)"
    }
}

/**
 * True when [url] names a tailnet node: an IPv4 in Tailscale's 100.64.0.0/10, an IPv6 in its fd7a:115c:a1e0::/48, or a
 * MagicDNS name (`*.ts.net`). A host reached this way is an ordinary pinned [Route.Direct] (the pairing link's `d` is
 * the node's tailnet address), but only with the Tailscale app running on the phone: the label says so. A literal
 * address is read, never resolved.
 */
fun isTailnetUrl(url: String): Boolean {
    val host = runCatching { java.net.URI(url.trim()).host }.getOrNull()?.lowercase()?.removePrefix("[")?.removeSuffix("]")?.removeSuffix(".") ?: return false
    if (host.endsWith(".ts.net")) return true
    if (host.startsWith("fd7a:115c:a1e0:")) return true
    val parts = host.split('.')
    if (parts.size != 4) return false
    val n = parts.map { p -> p.takeIf { it.length in 1..3 && it.all(Char::isDigit) }?.toInt()?.takeIf { it in 0..255 } ?: return false }
    return n[0] == 100 && n[1] in 64..127
}

/** Fingerprints are compared as lowercase hex without separators, however they were written (`AB:CD`, `abcd`). */
object Fingerprint {
    /** Also takes the daemon's own spelling, `sha256:<hex>`. */
    fun normalize(s: String): String = s.trim().removePrefix("sha256:").removePrefix("SHA256:").filter { it.isLetterOrDigit() }.lowercase()

    fun of(cert: X509Certificate): String =
        MessageDigest.getInstance("SHA-256").digest(cert.encoded).joinToString("") { "%02x".format(it) }

    /** True when [s] reads as a SHA-256 fingerprint: 64 hex digits, separators allowed. */
    fun isValid(s: String): Boolean = normalize(s).let { it.length == 64 && it.all { c -> c in '0'..'9' || c in 'a'..'f' } }
}

/**
 * Accepts exactly the certificate a pairing code (or the user) named, by its SHA-256, and nothing else: no
 * certificate authority is consulted, so a pinned host needs no certificate a public CA signed, and no other
 * certificate is accepted for it. Expiry is still checked.
 */
class PinnedTrust(pin: String) : X509TrustManager {
    private val pin = Fingerprint.normalize(pin)

    override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
        val leaf = chain?.firstOrNull() ?: throw CertificateException("the host sent no certificate")
        leaf.checkValidity()
        val got = Fingerprint.of(leaf)
        if (got != pin) throw CertificateException("the host's certificate $got is not the pinned one")
    }

    override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {
        throw CertificateException("client certificates are not used")
    }

    override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
}
