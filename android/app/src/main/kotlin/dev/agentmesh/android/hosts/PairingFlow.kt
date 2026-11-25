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
                // The host answered and said no: the same token fails on the other route too.
                if (e.code == Code.UNAUTHENTICATED || e.code == Code.INVALID_ARGUMENT || e.code == Code.PERMISSION_DENIED) throw PairingException(refusal(e))
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
            val existing = repo.hosts.value.firstOrNull { it.daemonId == got.hostId }
            if (existing != null) {
                // The same host paired again (a revoked device, a new relay): its entry gets the new routes and token.
                repo.update(existing.id, existing.name, routes, got.deviceToken)
                repo.get(existing.id)!!
            } else {
                repo.add(name, routes, got.deviceToken, daemonId = got.hostId)
            }
        } catch (e: Exception) {
            throw PairingException("The host paired this device, but the app couldn't save it (${e.message ?: e.javaClass.simpleName}). The code is used up: make a new one.")
        }
    }

    private fun label(r: Route) = when (r) { is Route.Direct -> "direct (${r.url})"; is Route.Relay -> "relay (${r.relayTcp})" }

    private fun refusal(e: ApiException): String {
        val m = e.message.orEmpty()
        return when {
            "expired" in m -> "This pairing code has expired. Make a new one on the host (meshctl pair)."
            "already used" in m -> "This pairing code was already used. Make a new one on the host (meshctl pair)."
            "unknown" in m -> "The host doesn't know this pairing code. It may be for another host, or it expired and was cleaned up: make a new one."
            else -> "The host refused the pairing: $m"
        }
    }
}
