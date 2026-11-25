package dev.agentmesh.android.net

/**
 * The seam for pairing by QR code or link (docs/apps.md, "Reaching a host"): the relay and pairing are built
 * separately, and fill this in. A pairing code carries the relay's URL, the host's id, the daemon certificate's
 * fingerprint and a one-time token; the app redeems it for a device token and a host with its routes. Nothing here reads
 * a code yet, so the add-host screen offers no pairing until [Pairing.available].
 */
data class PairingPayload(
    val name: String, val hostId: String, val pinSha256: String, val token: String,
    val directUrl: String? = null, val relayTcp: String? = null, val relayClientToken: String? = null,
) {
    /** The routes the code offers, direct first. */
    fun routes(): List<Route> = listOfNotNull(
        directUrl?.let { Route.Direct(it, pinSha256) },
        if (relayTcp != null && relayClientToken != null) Route.Relay(relayTcp, hostId, relayClientToken, pinSha256) else null,
    )
}

interface Pairing {
    /** True once this build can read a pairing code (the relay's format is known). */
    val available: Boolean

    /** The payload of a scanned code or an opened link, or null when it isn't one. */
    fun parse(text: String): PairingPayload?
}

object NoPairing : Pairing {
    override val available = false
    override fun parse(text: String): PairingPayload? = null
}
