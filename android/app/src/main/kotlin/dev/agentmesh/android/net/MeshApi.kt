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
import dev.agentmesh.gen.agentmesh.v1.Cursor
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
val UI_ACTIONS = listOf("hello", "subscribeEvents", "listSettings", "setSetting", "resetSetting")

/** The calls themselves, so a test can stand in for a host. [ConnectMeshApi] is the real one. */
interface MeshApi {
    suspend fun hello(): HelloResponse

    /** The event log from [cursor] (a snapshot when null), then live; ends when the stream closes, or throws. */
    fun subscribe(cursor: Cursor?): Flow<SubscribeEventsResponse>

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

class ConnectMeshApi(connection: Connection, private val clientName: String = "agent-mesh-android") : MeshApi {
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
