package dev.agentmesh.android.hosts

import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.relayRefusal
import kotlin.coroutines.cancellation.CancellationException

/** How asking a host to forget this device went. */
sealed interface UnpairResult {
    data object Done : UnpairResult
    data class Failed(val message: String) : UnpairResult
}

/**
 * Asks the host to revoke this device's own pairing (`RevokeSelf`), before the app forgets the host: otherwise the
 * device token stays valid on the host until its owner revokes it by hand. [api] is null while no connection was built.
 * A host that no longer knows the token (UNAUTHENTICATED), or a relay that refuses the revoked relay token at the
 * connection (a retry after a RevokeSelf whose answer was lost), has nothing left to revoke, so that counts as done;
 * a permission refusal of the call itself does not.
 */
suspend fun unpairFromHost(api: MeshApi?): UnpairResult {
    if (api == null) return UnpairResult.Failed("not connected to the host")
    return try {
        api.revokeSelf()
        UnpairResult.Done
    } catch (e: CancellationException) {
        throw e
    } catch (e: Exception) {
        when {
            relayRefusal(e)?.code == "auth" -> UnpairResult.Done
            e is ApiException && e.code == Code.UNAUTHENTICATED -> UnpairResult.Done
            e is ApiException -> UnpairResult.Failed(e.message ?: e.code.name.lowercase())
            else -> UnpairResult.Failed(e.message ?: e.javaClass.simpleName)
        }
    }
}
