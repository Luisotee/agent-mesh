package dev.agentmesh.android.hosts

import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.MeshApi
import kotlin.coroutines.cancellation.CancellationException

/** How asking a host to forget this device went. */
sealed interface UnpairResult {
    data object Done : UnpairResult
    data class Failed(val message: String) : UnpairResult
}

/**
 * Asks the host to revoke this device's own pairing (`RevokeSelf`), before the app forgets the host: otherwise the
 * device token stays valid on the host until its owner revokes it by hand. [api] is null while no connection was built.
 * A host that no longer knows the token (UNAUTHENTICATED) has nothing left to revoke, so that counts as done; a permission refusal of the call itself does not.
 */
suspend fun unpairFromHost(api: MeshApi?): UnpairResult {
    if (api == null) return UnpairResult.Failed("not connected to the host")
    return try {
        api.revokeSelf()
        UnpairResult.Done
    } catch (e: CancellationException) {
        throw e
    } catch (e: ApiException) {
        if (e.code == Code.UNAUTHENTICATED) UnpairResult.Done else UnpairResult.Failed(e.message ?: e.code.name.lowercase())
    } catch (e: Exception) {
        UnpairResult.Failed(e.message ?: e.javaClass.simpleName)
    }
}
