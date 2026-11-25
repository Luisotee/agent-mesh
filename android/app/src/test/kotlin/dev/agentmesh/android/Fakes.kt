package dev.agentmesh.android

import com.google.protobuf.Value
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.gen.agentmesh.v1.Cursor
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.ListMessagesResponse
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.Setting
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.emptyFlow

/** A secret box that seals nothing: tests don't have the Keystore. */
object Plain : dev.agentmesh.android.hosts.SecretBox {
    override fun seal(plain: String) = plain
    override fun open(sealed: String) = sealed
}

/**
 * A host that records what the app asks of it. Its handshake never answers (the screens are driven by the state a
 * test puts in), and its message pages are whatever the test gives it.
 */
class RecordingApi : MeshApi {
    val sent = mutableListOf<Triple<String, MessageType, String>>()
    val approved = mutableListOf<String>()
    val listCalls = mutableListOf<Pair<String, String>>()
    /** Pages by run: each call returns the next one, `more` set while pages remain. */
    var pages: Map<String, List<List<Message>>> = emptyMap()
    var failListings = 0
    /** Runs whose listing fails (the others work). */
    var failRuns: Set<String> = emptySet()
    /** When set, the next listing waits for it (a load in flight). */
    var listGate: CompletableDeferred<Unit>? = null
    var failSend: Exception? = null

    override suspend fun hello(): HelloResponse = CompletableDeferred<HelloResponse>().await()
    override fun subscribe(cursor: Cursor?): Flow<SubscribeEventsResponse> = emptyFlow()

    override suspend fun listMessages(runId: String, afterId: String): ListMessagesResponse {
        listCalls += runId to afterId
        listGate?.let { listGate = null; it.await() }
        if (runId in failRuns) throw ApiException(com.connectrpc.Code.UNAVAILABLE, "down")
        if (failListings > 0) { failListings--; throw ApiException(com.connectrpc.Code.UNAVAILABLE, "down") }
        val all = pages[runId].orEmpty()
        // The page after `afterId`: the one that follows the page whose last message that is.
        val idx = if (afterId.isEmpty()) 0 else all.indexOfFirst { p -> p.lastOrNull()?.id == afterId } + 1
        val page = all.getOrNull(idx).orEmpty()
        return ListMessagesResponse.newBuilder().addAllMessages(page).setMore(idx + 1 < all.size).build()
    }

    override suspend fun sendMessage(to: String, type: MessageType, body: String): Message {
        failSend?.let { throw it }
        sent += Triple(to, type, body)
        return message("sent-${sent.size}", from = "me", to = to, type = type, body = body, at = 90_000L + sent.size)
    }

    override suspend fun approveAgent(agent: String): Message {
        approved += agent
        return message("approved-${approved.size}", from = "me", to = agent, type = MessageType.MESSAGE_TYPE_ANSWER, body = "approved", at = 90_000L + approved.size)
    }

    override suspend fun listSettings(prefix: String): List<Setting> = emptyList()
    override suspend fun setSetting(key: String, value: Value): Setting = error("unused")
    override suspend fun resetSetting(key: String): Setting = error("unused")
}
