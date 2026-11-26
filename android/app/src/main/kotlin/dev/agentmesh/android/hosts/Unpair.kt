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
 * A host that no longer knows the token (UNAUTHENTICATED) has nothing left to revoke, so that counts as done; a
 * permission refusal of the call itself does not. A relay that refuses the relay token at the connection counts as done
 * only for a retry ([afterFailedTry]: the first try may have revoked the device and lost its answer): the relay answers
 * the same for a revoked token as for one it doesn't know, so on a first try it is a failure the user decides about.
 */
suspend fun unpairFromHost(api: MeshApi?, afterFailedTry: Boolean = false): UnpairResult {
    if (api == null) return UnpairResult.Failed("not connected to the host")
    return try {
        api.revokeSelf()
        UnpairResult.Done
    } catch (e: CancellationException) {
        throw e
    } catch (e: Exception) {
        when {
            afterFailedTry && relayRefusal(e)?.code == "auth" -> UnpairResult.Done
            e is ApiException && e.code == Code.UNAUTHENTICATED -> UnpairResult.Done
            e is ApiException -> UnpairResult.Failed(e.message ?: e.code.name.lowercase())
            else -> UnpairResult.Failed(e.message ?: e.javaClass.simpleName)
        }
    }
}
