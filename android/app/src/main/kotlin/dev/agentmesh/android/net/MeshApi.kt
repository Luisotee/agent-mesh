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
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentPullRequests
import dev.agentmesh.gen.agentmesh.v1.CheckModelsRequest
import dev.agentmesh.gen.agentmesh.v1.CheckModelsResponse
import dev.agentmesh.gen.agentmesh.v1.GetCostRequest
import dev.agentmesh.gen.agentmesh.v1.GetCostResponse
import dev.agentmesh.gen.agentmesh.v1.GetUsageRequest
import dev.agentmesh.gen.agentmesh.v1.GetUsageResponse
import dev.agentmesh.gen.agentmesh.v1.HoldPullRequestRequest
import dev.agentmesh.gen.agentmesh.v1.ListAgentsRequest
import dev.agentmesh.gen.agentmesh.v1.ListPullRequestsRequest
import dev.agentmesh.gen.agentmesh.v1.ListWaitingRequest
import dev.agentmesh.gen.agentmesh.v1.PromptAgentRequest
import dev.agentmesh.gen.agentmesh.v1.PullRequest
import dev.agentmesh.gen.agentmesh.v1.ReadTranscriptRequest
import dev.agentmesh.gen.agentmesh.v1.ReadTranscriptResponse
import dev.agentmesh.gen.agentmesh.v1.SwitchAgentRequest
import dev.agentmesh.gen.agentmesh.v1.SwitchAgentResponse
import dev.agentmesh.gen.agentmesh.v1.Waiting
import dev.agentmesh.gen.agentmesh.v1.PushRegistration
import dev.agentmesh.gen.agentmesh.v1.RegisterPushRequest
import dev.agentmesh.gen.agentmesh.v1.TestPushRequest
import dev.agentmesh.gen.agentmesh.v1.UnregisterPushRequest
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
import dev.agentmesh.gen.agentmesh.v1.PairingServiceClient
import dev.agentmesh.gen.agentmesh.v1.RedeemPairingRequest
import dev.agentmesh.gen.agentmesh.v1.MeshServiceClient
import dev.agentmesh.gen.agentmesh.v1.RequestMeta
import dev.agentmesh.gen.agentmesh.v1.ResetSettingRequest
import dev.agentmesh.gen.agentmesh.v1.RevokeSelfRequest
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
val UI_ACTIONS = listOf(
    "hello", "subscribeEvents", "listSettings", "setSetting", "resetSetting", "listMessages", "sendMessage", "approveAgent", "openTerminal",
    "registerPush", "unregisterPush", "testPush", "revokeSelf",
    "readTranscript", "promptAgent", "switchAgent", "checkModels", "listAgents", "listWaiting", "listPullRequests", "holdPullRequest", "getUsage", "getCost",
)

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

    /** Revokes this device's own pairing on the host: its token stops working there, and its streams are closed. */
    suspend fun revokeSelf()

    suspend fun listSettings(prefix: String = ""): List<Setting>
    suspend fun setSetting(key: String, value: Value): Setting
    suspend fun resetSetting(key: String): Setting

    /** Gives the host this device's UnifiedPush endpoint and its WebPush keys (base64url), so it pushes here, and the kinds of notice this device chose to be pushed ([events]; none follows the host's list). */
    suspend fun registerPush(endpoint: String, p256dh: String, auth: String, events: List<String> = emptyList()): PushRegistration

    /** Tells the host to stop pushing to this device. Returns whether it had an endpoint. */
    suspend fun unregisterPush(): Boolean

    /** Has the host send this device a test push now; throws with the push service's error when it fails. */
    suspend fun testPush(): PushRegistration

    /**
     * A page of an agent's conversation (its CLI's record). [offset] is a position from a previous answer, -1 the end; with
     * [backward] the entries before it, else after it.
     */
    suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): ReadTranscriptResponse

    /** Types [text] into the running agent as the user would (the host refuses while its input box holds a draft). */
    suspend fun promptAgent(agent: String, text: String)

    /** Reopens the agent's conversation on another tier, a model, or an effort alone; the host's refusal says why not. */
    suspend fun switchAgent(agent: String, tier: String = "", model: String = "", effort: String = ""): SwitchAgentResponse

    /** The model families each CLI lists on the host (a day-old answer unless the host checks again). */
    suspend fun checkModels(): CheckModelsResponse

    /** Every agent as the host derives it now (a quiet agent shows as unknown only here). */
    suspend fun listAgents(): List<Agent>

    /** What waits on the user, as the host says (`mesh waiting`). */
    suspend fun listWaiting(): List<Waiting>

    /** The pull requests of the change workers of [runId], from GitHub (the host caches two minutes). */
    suspend fun listPullRequests(runId: String): List<AgentPullRequests>

    /** Holds a PR's merge (or with [release] lets it go); the PR as recorded now, once for each agent that owns it. */
    suspend fun holdPullRequest(url: String, release: Boolean): List<PullRequest>

    suspend fun getUsage(refresh: Boolean = false): GetUsageResponse

    /** What `mesh cost` prints: for a run, or (with [since], a span back from now such as `7d`) a window. */
    suspend fun getCost(runId: String = "", since: String = "", agents: Boolean = false): GetCostResponse
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
            // Also the ephemeral activity frames (the tool an agent runs now), which the live dock and the rows show.
            val req = SubscribeEventsRequest.newBuilder().setActivity(true).apply { if (cursor != null) setCursor(cursor) }.build()
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

    override suspend fun revokeSelf() {
        service.revokeSelf(RevokeSelfRequest.newBuilder().setMeta(newMeta()).build()).ok()
    }

    override suspend fun listSettings(prefix: String): List<Setting> =
        service.listSettings(ListSettingsRequest.newBuilder().setPrefix(prefix).build()).ok().settingsList

    override suspend fun setSetting(key: String, value: Value): Setting =
        service.setSetting(SetSettingRequest.newBuilder().setMeta(newMeta()).setKey(key).setValue(value).build()).ok().setting

    override suspend fun resetSetting(key: String): Setting =
        service.resetSetting(ResetSettingRequest.newBuilder().setMeta(newMeta()).setKey(key).build()).ok().setting

    override suspend fun registerPush(endpoint: String, p256dh: String, auth: String, events: List<String>): PushRegistration =
        service.registerPush(RegisterPushRequest.newBuilder().setMeta(newMeta()).setEndpoint(endpoint).setP256Dh(p256dh).setAuth(auth).addAllEvents(events).build()).ok().registration

    override suspend fun unregisterPush(): Boolean =
        service.unregisterPush(UnregisterPushRequest.newBuilder().setMeta(newMeta()).build()).ok().removed

    override suspend fun testPush(): PushRegistration =
        service.testPush(TestPushRequest.newBuilder().setMeta(newMeta()).build()).ok().registration

    override suspend fun readTranscript(agent: String, offset: Long, backward: Boolean): ReadTranscriptResponse =
        service.readTranscript(ReadTranscriptRequest.newBuilder().setAgent(agent).setOffset(offset).setBackward(backward).build()).ok()

    override suspend fun promptAgent(agent: String, text: String) {
        service.promptAgent(PromptAgentRequest.newBuilder().setMeta(newMeta()).setAgent(agent).setText(text).build()).ok()
    }

    override suspend fun switchAgent(agent: String, tier: String, model: String, effort: String): SwitchAgentResponse =
        service.switchAgent(SwitchAgentRequest.newBuilder().setMeta(newMeta()).setAgent(agent).setTier(tier).setModel(model).setEffort(effort).build()).ok()

    override suspend fun checkModels(): CheckModelsResponse =
        service.checkModels(CheckModelsRequest.newBuilder().setMeta(newMeta()).build()).ok()

    override suspend fun listAgents(): List<Agent> = service.listAgents(ListAgentsRequest.getDefaultInstance()).ok().agentsList

    override suspend fun listWaiting(): List<Waiting> = service.listWaiting(ListWaitingRequest.getDefaultInstance()).ok().waitingList

    override suspend fun listPullRequests(runId: String): List<AgentPullRequests> =
        service.listPullRequests(ListPullRequestsRequest.newBuilder().setRunId(runId).build()).ok().agentsList

    override suspend fun holdPullRequest(url: String, release: Boolean): List<PullRequest> =
        service.holdPullRequest(HoldPullRequestRequest.newBuilder().setMeta(newMeta()).setUrl(url).setRelease(release).build()).ok().pullRequestsList

    override suspend fun getUsage(refresh: Boolean): GetUsageResponse = service.getUsage(GetUsageRequest.newBuilder().setRefresh(refresh).build()).ok()

    override suspend fun getCost(runId: String, since: String, agents: Boolean): GetCostResponse =
        service.getCost(GetCostRequest.newBuilder().setRunId(runId).setSince(since).setAgents(agents).build()).ok()

    private fun Throwable.asApiException(): ApiException = when (this) {
        is ApiException -> this
        is ConnectException -> ApiException(code, message ?: code.name.lowercase(), this)
        else -> ApiException(Code.UNAVAILABLE, message ?: javaClass.simpleName, this)
    }
}

/** [PairingApi] over a connection that carries no token (`PairingService` is the one service that needs none). */
class ConnectPairingApi(connection: Connection) : PairingApi {
    private val pairingService = PairingServiceClient(
        ProtocolClient(
            ConnectOkHttpClient(connection.http),
            ProtocolClientConfig(
                host = connection.baseUrl,
                serializationStrategy = GoogleJavaLiteProtobufStrategy(),
                networkProtocol = NetworkProtocol.CONNECT,
                timeoutOracle = simpleTimeouts(unaryTimeout = 20.seconds, streamTimeout = null),
            ),
        ),
    )

    override suspend fun redeem(token: String, deviceName: String): Redeemed =
        when (val r = pairingService.redeemPairing(RedeemPairingRequest.newBuilder().setToken(token).setDeviceName(deviceName).build())) {
            is ResponseMessage.Success -> r.message.let { Redeemed(it.deviceId, it.deviceName, it.deviceToken, it.hostId, it.hostName, it.certFingerprint) }
            is ResponseMessage.Failure -> throw ApiException(r.cause.code, r.cause.message ?: r.cause.code.name.lowercase(), r.cause)
        }
}
