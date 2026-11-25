package dev.agentmesh.android.net

import com.connectrpc.Code
import com.connectrpc.ConnectException
import com.connectrpc.ProtocolClientConfig
import com.connectrpc.ResponseMessage
import com.connectrpc.extensions.GoogleJavaLiteProtobufStrategy
import com.connectrpc.simpleTimeouts
import com.connectrpc.impl.ProtocolClient
import com.connectrpc.okhttp.ConnectOkHttpClient
import com.connectrpc.protocols.NetworkProtocol
import dev.agentmesh.gen.agentmesh.v1.ApproveAgentRequest
import dev.agentmesh.gen.agentmesh.term.v1.TermFrame
import dev.agentmesh.gen.agentmesh.v1.Cursor
import dev.agentmesh.gen.agentmesh.v1.OpenTerminalRequest
import kotlinx.coroutines.channels.ReceiveChannel
import dev.agentmesh.gen.agentmesh.v1.ListMessagesRequest
import dev.agentmesh.gen.agentmesh.v1.ListMessagesResponse
import dev.agentmesh.gen.agentmesh.v1.Message
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.SendMessageRequest
import dev.agentmesh.gen.agentmesh.v1.HelloRequest
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.ListSettingsRequest
import dev.agentmesh.gen.agentmesh.v1.MeshServiceClient
import dev.agentmesh.gen.agentmesh.v1.RequestMeta
import dev.agentmesh.gen.agentmesh.v1.ResetSettingRequest
import dev.agentmesh.gen.agentmesh.v1.SetSettingRequest
import dev.agentmesh.gen.agentmesh.v1.Setting
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsRequest
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import com.google.protobuf.Timestamp
import com.google.protobuf.Value
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import java.util.UUID
import kotlin.time.Duration.Companion.seconds

/** The API major version this app speaks (the daemon's `ProtocolVersion`). */
const val PROTOCOL_VERSION = 1

/** A call that failed, with the ConnectRPC code. */
class ApiException(val code: Code, message: String, cause: Throwable? = null) : Exception(message, cause) {
    /** The host refused the token: retrying with it can't help. */
    val refusedToken: Boolean get() = code == Code.UNAUTHENTICATED || code == Code.PERMISSION_DENIED
}

/**
 * Every call the app makes to a host, by registry method name. `RegistryPinTest` checks that each one is an
 * action a device may call (proto/agentmesh/v1/mesh.proto): the app can do nothing an agent can't do through the
 * same registry.
 */
val UI_ACTIONS = listOf("hello", "subscribeEvents", "listSettings", "setSetting", "resetSetting", "listMessages", "sendMessage", "approveAgent", "openTerminal")

/** What a terminal stream delivers: frames, then how it ended. */
sealed interface TermEvent {
    class Frame(val frame: TermFrame) : TermEvent

    /** The host closed the stream with [code] (4000 exited, 4001 slow, 4002 shutdown, 4003 bad request). */
    class Closed(val code: Int, val reason: String) : TermEvent

    /** The connection failed or broke. */
    class Failed(val cause: Throwable) : TermEvent
}

/** One terminal stream. [events] ends after a [TermEvent.Closed] or [TermEvent.Failed]. */
interface TerminalSocket {
    val events: ReceiveChannel<TermEvent>
    fun send(frame: TermFrame): Boolean
    fun close()
}

/** The calls themselves, so a test can stand in for a host. [ConnectMeshApi] is the real one. */
interface MeshApi {
    suspend fun hello(): HelloResponse

    /** The event log from [cursor] (a snapshot when null), then live; ends when the stream closes, or throws. */
    fun subscribe(cursor: Cursor?): Flow<SubscribeEventsResponse>

    /** One page of a run's messages, oldest first, after the message with [afterId] (empty: from the start). */
    suspend fun listMessages(runId: String, afterId: String): ListMessagesResponse

    suspend fun sendMessage(to: String, type: MessageType, body: String): Message

    /** Approves an agent's plan: it moves to its execution model in the same conversation. Returns the stored answer. */
    suspend fun approveAgent(agent: String): Message

    /** A one-time ticket for an agent's terminal stream: the path to open on the host (read only: this app asks for no input right). */
    suspend fun openTerminal(agent: String): String

    /** The terminal stream at [path] (from [openTerminal]): a WebSocket of binary `TermFrame`s, both ways. */
    fun connectTerminal(path: String): TerminalSocket

    suspend fun listSettings(prefix: String = ""): List<Setting>
    suspend fun setSetting(key: String, value: Value): Setting
    suspend fun resetSetting(key: String): Setting
}

/** A write's idempotency and expiry: a retried or delayed call neither runs twice nor runs late. */
fun newMeta(nowMs: Long = System.currentTimeMillis(), ttlMs: Long = 60_000): RequestMeta {
    val at = nowMs + ttlMs
    return RequestMeta.newBuilder()
        .setMutationId(UUID.randomUUID().toString())
        .setExpiresAt(Timestamp.newBuilder().setSeconds(at / 1000).setNanos(((at % 1000) * 1_000_000).toInt()))
        .build()
}

private const val PAGE = 500

class ConnectMeshApi(private val connection: Connection, private val clientName: String = "agent-mesh-android") : MeshApi {
    private val service = MeshServiceClient(
        ProtocolClient(
            ConnectOkHttpClient(connection.http),
            ProtocolClientConfig(
                host = connection.baseUrl,
                serializationStrategy = GoogleJavaLiteProtobufStrategy(),
                networkProtocol = NetworkProtocol.CONNECT,
                // A call is bounded; the event stream isn't (HostConnection's heartbeat is what notices a dead one).
                timeoutOracle = simpleTimeouts(unaryTimeout = 10.seconds, streamTimeout = null),
            ),
        ),
    )

    private fun <T> ResponseMessage<T>.ok(): T = when (this) {
        is ResponseMessage.Success -> message
        is ResponseMessage.Failure -> throw ApiException(cause.code, cause.message ?: cause.code.name.lowercase(), cause)
    }

    override suspend fun hello(): HelloResponse =
        service.hello(HelloRequest.newBuilder().setClient(clientName).setProtocolVersion(PROTOCOL_VERSION).build()).ok()

    override fun subscribe(cursor: Cursor?): Flow<SubscribeEventsResponse> = flow {
        val stream = service.subscribeEvents()
        try {
            val req = SubscribeEventsRequest.newBuilder().apply { if (cursor != null) setCursor(cursor) }.build()
            stream.sendAndClose(req).exceptionOrNull()?.let { throw it.asApiException() }
            try {
                for (frame in stream.responseChannel()) emit(frame)
            } catch (e: ConnectException) {
                throw ApiException(e.code, e.message ?: e.code.name.lowercase(), e)
            }
        } finally {
            stream.receiveClose()
        }
    }

    override suspend fun listMessages(runId: String, afterId: String): ListMessagesResponse =
        service.listMessages(ListMessagesRequest.newBuilder().setRunId(runId).setAfterId(afterId).setLimit(PAGE).build()).ok()

    override suspend fun sendMessage(to: String, type: MessageType, body: String): Message =
        service.sendMessage(SendMessageRequest.newBuilder().setMeta(newMeta()).setTo(to).setType(type).setBody(body).build()).ok().message

    override suspend fun approveAgent(agent: String): Message =
        service.approveAgent(ApproveAgentRequest.newBuilder().setMeta(newMeta()).setAgent(agent).build()).ok().message

    override suspend fun openTerminal(agent: String): String =
        service.openTerminal(OpenTerminalRequest.newBuilder().setAgent(agent).setInput(false).build()).ok().path

    override fun connectTerminal(path: String): TerminalSocket = OkHttpTerminalSocket(connection, path)

    override suspend fun listSettings(prefix: String): List<Setting> =
        service.listSettings(ListSettingsRequest.newBuilder().setPrefix(prefix).build()).ok().settingsList

    override suspend fun setSetting(key: String, value: Value): Setting =
        service.setSetting(SetSettingRequest.newBuilder().setMeta(newMeta()).setKey(key).setValue(value).build()).ok().setting

    override suspend fun resetSetting(key: String): Setting =
        service.resetSetting(ResetSettingRequest.newBuilder().setMeta(newMeta()).setKey(key).build()).ok().setting

    private fun Throwable.asApiException(): ApiException = when (this) {
        is ApiException -> this
        is ConnectException -> ApiException(code, message ?: code.name.lowercase(), this)
        else -> ApiException(Code.UNAVAILABLE, message ?: javaClass.simpleName, this)
    }
}
