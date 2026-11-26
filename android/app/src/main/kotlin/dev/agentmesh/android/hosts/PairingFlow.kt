package dev.agentmesh.android.hosts

import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.DefaultRouteConnector
import dev.agentmesh.android.net.ConnectPairingApi
import dev.agentmesh.android.net.Fingerprint
import dev.agentmesh.android.net.PairingApi
import dev.agentmesh.android.net.PairingException
import dev.agentmesh.android.net.PairingPayload
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.net.RouteException
import dev.agentmesh.android.net.describeFailure
import dev.agentmesh.android.net.relayRefusal
import kotlinx.coroutines.CancellationException

/** How a pairing call reaches a route: the real one builds a pinned client without a token; a test stands in. */
fun interface PairingClients {
    fun open(route: Route): PairingApi

    companion object {
        val Default = PairingClients { ConnectPairingApi(DefaultRouteConnector().anonymous(it)) }
    }
}

/**
 * Pairing (docs/relay.md, "Pairing flow"): the code's one-time token is redeemed on its first route that answers
 * (direct first, then the relay), for a device token; the host is saved with the code's routes and pin, and the
 * daemon's id, so another daemon at the same address is noticed. A code works once: a failure after the host
 * accepted it (a lost reply, a full disk) means the host must make a new one, and the message says so.
 */
class PairingFlow(private val repo: HostRepository, private val clients: PairingClients = PairingClients.Default) {
    suspend fun pair(payload: PairingPayload, deviceName: String): Host {
        val routes = payload.routes()
        val failures = mutableListOf<String>()
        var redeemed: dev.agentmesh.android.net.Redeemed? = null
        for (route in routes) {
            try {
                redeemed = clients.open(route).redeem(payload.token, deviceName.trim().ifEmpty { payload.deviceName })
                break
            } catch (e: CancellationException) {
                throw e
            } catch (e: ApiException) {
                // The host answered and said no (the relay's own refusals are not the host's): the same token fails on the other route too.
                if (relayRefusal(e) == null && e.code !in NOT_AN_ANSWER) {
                    throw PairingException(refusal(e, earlierFailures = failures))
                }
                failures += "${label(route)}: ${describeFailure(e)}"
            } catch (e: RouteException) {
                failures += "${label(route)}: ${e.message}"
            } catch (e: Exception) {
                failures += "${label(route)}: ${describeFailure(e)}"
            }
        }
        val got = redeemed ?: throw PairingException("Couldn't reach the host. ${failures.joinToString(" ")}")
        // The TLS pin already named the host; this is the host agreeing with the code about who it is.
        if (got.hostId != payload.hostId || Fingerprint.normalize(got.certFingerprint) != payload.pinSha256) {
            throw PairingException("The host that answered isn't the one in the code (the code was used up: make a new one).")
        }
        val name = payload.hostName.ifEmpty { got.hostName }.ifEmpty { "Host" }
        return try {
            // The same host: by the daemon's id, or (a host added by hand has none yet) by an address the code also names.
            val existing = repo.hosts.value.firstOrNull { it.daemonId == got.hostId }
                ?: repo.hosts.value.firstOrNull { h -> h.daemonId == null && h.routes.any { r -> routes.any { sameRoute(r, it, got.hostId) } } }
            if (existing != null) {
                // The same host paired again (a revoked device, a new relay): its entry gets the new routes and token.
                repo.update(existing.id, existing.name, mergeRoutes(existing.routes, routes), got.deviceToken)
                repo.setDaemonId(existing.id, got.hostId)
                repo.get(existing.id)!!
            } else {
                repo.add(name, routes, got.deviceToken, daemonId = got.hostId)
            }
        } catch (e: Exception) {
            throw PairingException("The host paired this device, but the app couldn't save it (${e.message ?: e.javaClass.simpleName}). The code is used up: make a new one.")
        }
    }

    private companion object {
        /** Codes that say the call never got an answer from the host (or not a real one): the next route is tried. */
        val NOT_AN_ANSWER = setOf(Code.UNKNOWN, Code.UNAVAILABLE, Code.DEADLINE_EXCEEDED, Code.CANCELED)
    }

    /**
     * The host's routes after pairing again: each route the code names replaces the one of the same kind and address (it may carry a new
     * client token), and every other route the host had (a Tailscale address added by hand) is kept. Routes only the code names come last.
     */
    private fun mergeRoutes(existing: List<Route>, fromCode: List<Route>): List<Route> {
        val result = existing.toMutableList()
        for (r in fromCode) {
            val at = result.indexOfFirst { sameRoute(it, r) }
            if (at >= 0) result[at] = r else result.add(r)
        }
        return result
    }

    private fun sameRoute(a: Route, b: Route): Boolean = when {
        a is Route.Direct && b is Route.Direct -> a.url.trim().trimEnd('/').equals(b.url.trim().trimEnd('/'), ignoreCase = true)
        a is Route.Relay && b is Route.Relay -> a.relayTcp.equals(b.relayTcp, ignoreCase = true)
        else -> false
    }

    private fun sameRoute(a: Route, b: Route, hostId: String): Boolean = when {
        a is Route.Direct && b is Route.Direct -> a.url.trim().trimEnd('/').equals(b.url.trim().trimEnd('/'), ignoreCase = true)
        a is Route.Relay && b is Route.Relay -> a.relayTcp.equals(b.relayTcp, ignoreCase = true) && a.hostId == hostId
        else -> false
    }

    private fun label(r: Route) = when (r) { is Route.Direct -> "direct (${r.url})"; is Route.Relay -> "relay (${r.relayTcp})" }

    private fun refusal(e: ApiException, earlierFailures: List<String>): String {
        val m = e.message.orEmpty()
        return when {
            // The host may have taken the token on a route whose reply never came back.
            "already used" in m && earlierFailures.isNotEmpty() ->
                "This pairing code is used up. An earlier try (${earlierFailures.first()}) may have reached the host before its reply was lost: make a new code on the host (mesh pair)."
            "expired" in m -> "This pairing code has expired. Make a new one on the host (mesh pair)."
            "already used" in m -> "This pairing code was already used. Make a new one on the host (mesh pair)."
            "unknown" in m -> "The host doesn't know this pairing code. It may be for another host, or it expired and was cleaned up: make a new one."
            else -> "The host refused the pairing: $m"
        }
    }
}
