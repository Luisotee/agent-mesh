package dev.agentmesh.android.net

import java.net.URLDecoder

/** A pairing code that can't be used, and why: shown to the user as it is. */
class PairingException(message: String) : Exception(message)

/**
 * What a pairing code carries (docs/relay.md, "Pairing flow and URI contract"): the host, its certificate's
 * fingerprint, a one-time token, and the routes to reach it. The token is a secret until it is redeemed: it stays
 * out of [toString], logs and saved state.
 */
data class PairingPayload(
    val hostId: String,
    val hostName: String,
    val pinSha256: String,
    val token: String,
    val directUrl: String? = null,
    val relayTcp: String? = null,
    val relayClientToken: String? = null,
    /** The device name the host suggests (`n`), if the code was made with one. */
    val deviceName: String = "",
    /** What the code offered that this app doesn't use (an `http://` address), so the user isn't left guessing. */
    val skipped: List<String> = emptyList(),
) {
    /** The routes the code offers, direct first. */
    fun routes(): List<Route> = listOfNotNull(
        directUrl?.let { Route.Direct(it, pinSha256) },
        if (relayTcp != null && relayClientToken != null) Route.Relay(relayTcp, hostId, relayClientToken, pinSha256) else null,
    )

    override fun toString() = "PairingPayload(hostId=$hostId, hostName=$hostName, routes=${routes().size})"
}

/** Reads `agentmesh://pair?v=1&h=…&fp=…&t=…[&hn][&r][&rtcp][&rt][&d][&n]`, as `mesh pair` and the QR code write it. */
object PairingLink {
    private const val PREFIX = "agentmesh://pair"

    /** The link inside [text] (a QR's content, a pasted message): from `agentmesh://pair` to the next whitespace. */
    fun find(text: String): String? {
        val i = text.indexOf(PREFIX, ignoreCase = true)
        if (i < 0) return null
        return text.substring(i).takeWhile { !it.isWhitespace() }.trimEnd('.', ',', ')', '>', '"', '\'')
    }

    fun parse(text: String): PairingPayload {
        val link = find(text) ?: throw PairingException("That isn't a pairing link: it should start with agentmesh://pair")
        val q = query(link)
        val v = q["v"]
        if (v != "1") throw PairingException(if (v == null) "The pairing link has no version" else "This pairing link is from a newer version of agent-mesh (v$v): update the app")
        val hostId = q["h"].orEmpty().trim()
        val token = q["t"].orEmpty().trim()
        if (hostId.isEmpty() || token.isEmpty()) throw PairingException("The pairing link is incomplete (no host or token)")
        val fp = q["fp"].orEmpty()
        if (!Fingerprint.isValid(fp)) throw PairingException("The pairing link has no valid certificate fingerprint")
        if (!isPreambleWord(hostId)) throw PairingException("The pairing link's host id has characters that can't be right (spaces or control characters)")
        val rawDirect = q["d"]?.trim()?.takeIf { it.isNotEmpty() }
        val direct = rawDirect?.takeIf { it.startsWith("https://", ignoreCase = true) }
        val skipped = listOfNotNull(rawDirect?.takeIf { direct == null }?.let { "the direct address $it (this app needs https:// for a host it pins)" })
        val rtcp = q["rtcp"]?.trim()?.takeIf { splitHostPort(it) != null }
        val rt = q["rt"]?.trim()?.takeIf { it.isNotEmpty() }
        if (rt != null && !isPreambleWord(rt)) throw PairingException("The pairing link's relay token has characters that can't be right (spaces or control characters)")
        val payload = PairingPayload(
            hostId = hostId, hostName = q["hn"].orEmpty().trim(), pinSha256 = Fingerprint.normalize(fp), token = token,
            directUrl = direct, relayTcp = if (rt != null) rtcp else null, relayClientToken = if (rtcp != null) rt else null, deviceName = q["n"].orEmpty().trim(), skipped = skipped,
        )
        if (payload.routes().isEmpty()) throw PairingException("The pairing link names no address this app can use (it needs an https:// address or a relay)" + skipped.joinToString("") { ": it offered $it" })
        return payload
    }

    private fun query(link: String): Map<String, String> {
        val q = link.substringAfter('?', "").substringBefore('#')
        val out = HashMap<String, String>()
        for (pair in q.split('&')) {
            if (pair.isEmpty()) continue
            val k = pair.substringBefore('=')
            val v = pair.substringAfter('=', "")
            // The first one wins, as url.Values.Get does.
            runCatching { out.putIfAbsent(URLDecoder.decode(k, "UTF-8"), URLDecoder.decode(v, "UTF-8")) }
        }
        return out
    }
}

/** What the host gave back for a pairing token. The device token is a secret: [toString] leaves it out. */
class Redeemed(
    val deviceId: String, val deviceName: String, val deviceToken: String,
    val hostId: String, val hostName: String, val certFingerprint: String,
) {
    override fun toString() = "Redeemed(deviceId=$deviceId, hostId=$hostId)"
}

/** The one call a device makes before it has a token: `PairingService/RedeemPairing`. */
interface PairingApi {
    suspend fun redeem(token: String, deviceName: String): Redeemed
}
