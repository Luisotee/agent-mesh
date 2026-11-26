package dev.agentmesh.android

import com.google.protobuf.Value
import dev.agentmesh.android.net.ApiException
import com.google.protobuf.ByteString
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.TermEvent
import dev.agentmesh.android.net.TerminalSocket
import dev.agentmesh.gen.agentmesh.term.v1.AttachMode
import dev.agentmesh.gen.agentmesh.term.v1.Attached
import dev.agentmesh.gen.agentmesh.term.v1.Exited
import dev.agentmesh.gen.agentmesh.term.v1.Output
import dev.agentmesh.gen.agentmesh.term.v1.ReplayDone
import dev.agentmesh.gen.agentmesh.term.v1.Screen
import dev.agentmesh.gen.agentmesh.term.v1.TermFrame
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.channels.ReceiveChannel
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

    /** Terminal streams the next attaches get, in order; each attach also asks for a ticket first. */
    val sockets = ArrayDeque<FakeSocket>()
    val tickets = mutableListOf<String>()
    var terminalError: Throwable? = null

    override suspend fun openTerminal(agent: String): String {
        terminalError?.let { throw it }
        tickets += agent
        return "/v1/term/$agent?ticket=t${tickets.size}"
    }

    override fun connectTerminal(path: String): TerminalSocket = sockets.removeFirstOrNull() ?: FakeSocket()

    override suspend fun listSettings(prefix: String): List<Setting> = emptyList()
    override suspend fun setSetting(key: String, value: Value): Setting = error("unused")
    override suspend fun resetSetting(key: String): Setting = error("unused")

    /** What push calls reached the host: "register <endpoint>", "unregister", "test". */
    val push = java.util.Collections.synchronizedList(mutableListOf<String>())
    var failPush: Exception? = null

    override suspend fun registerPush(endpoint: String, p256dh: String, auth: String): dev.agentmesh.gen.agentmesh.v1.PushRegistration {
        failPush?.let { throw it }
        push += "register $endpoint"
        return dev.agentmesh.gen.agentmesh.v1.PushRegistration.getDefaultInstance()
    }

    /** While set, UnregisterPush hangs until it completes: a host that is slow to answer. */
    @Volatile var unregisterGate: kotlinx.coroutines.CompletableDeferred<Unit>? = null

    override suspend fun unregisterPush(): Boolean {
        unregisterGate?.await()
        push += "unregister"
        return true
    }

    override suspend fun testPush(): dev.agentmesh.gen.agentmesh.v1.PushRegistration {
        failPush?.let { throw it }
        push += "test"
        return dev.agentmesh.gen.agentmesh.v1.PushRegistration.getDefaultInstance()
    }
}

/** A terminal stream a test scripts: what the host sends is put in with [host], what the app sent is in [sent]. */
class FakeSocket : TerminalSocket {
    private val channel = Channel<TermEvent>(Channel.UNLIMITED)
    val sent = mutableListOf<TermFrame>()
    var closed = false
    override val events: ReceiveChannel<TermEvent> get() = channel
    override fun send(frame: TermFrame): Boolean { sent += frame; return true }
    override fun close() { closed = true; channel.close() }

    fun host(f: TermFrame) { channel.trySend(TermEvent.Frame(f)) }
    fun close(code: Int, reason: String = "") { channel.trySend(TermEvent.Closed(code, reason)); channel.close() }
    fun fail(e: Throwable) { channel.trySend(TermEvent.Failed(e)); channel.close() }
}

object Frames {
    fun attached(mode: AttachMode, epoch: Long = 1, offset: Long = 0, cols: Int = 40, rows: Int = 10, reanchored: Boolean = false) =
        TermFrame.newBuilder().setAttached(Attached.newBuilder().setMode(mode).setEpoch(epoch).setOffset(offset).setCols(cols).setRows(rows).setReanchored(reanchored)).build()

    fun screen(text: String, epoch: Long = 1, offset: Long = text.length.toLong()) =
        TermFrame.newBuilder().setScreen(Screen.newBuilder().setEpoch(epoch).setOffset(offset).setData(ByteString.copyFromUtf8(text))).build()

    fun output(text: String, offset: Long, epoch: Long = 1) =
        TermFrame.newBuilder().setOutput(Output.newBuilder().setEpoch(epoch).setOffset(offset).setData(ByteString.copyFromUtf8(text))).build()

    fun replayDone(offset: Long = 0) = TermFrame.newBuilder().setReplayDone(ReplayDone.newBuilder().setOffset(offset)).build()
    fun exited(code: Int = 0, signal: Int = 0) = TermFrame.newBuilder().setExited(Exited.newBuilder().setExitCode(code).setSignal(signal)).build()
}

/** A distributor a test drives: what the app registered, and whether one is installed and chosen. */
class FakePushChannel : dev.agentmesh.android.notify.PushChannel {
    var installed = listOf("io.heckel.ntfy")
    var chosen: String? = null
    val registered = java.util.Collections.synchronizedList(mutableListOf<String>())
    val unregistered = java.util.Collections.synchronizedList(mutableListOf<String>())
    override fun distributors() = installed
    override fun current() = chosen
    override fun pick(activity: android.app.Activity, done: (Boolean) -> Unit) {
        chosen = installed.firstOrNull()
        done(chosen != null)
    }
    override fun register(hostId: String, hostName: String) { registered += hostId }
    override fun unregister(hostId: String) { unregistered += hostId }
}

/** Endpoints kept in memory. */
class MemoryEndpoints : dev.agentmesh.android.notify.EndpointStore {
    val map = java.util.concurrent.ConcurrentHashMap<String, dev.agentmesh.android.notify.SavedEndpoint>()
    override fun get(hostId: String) = map[hostId]
    override fun put(hostId: String, e: dev.agentmesh.android.notify.SavedEndpoint) { map[hostId] = e }
    override fun remove(hostId: String) { map.remove(hostId) }
}
