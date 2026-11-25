package dev.agentmesh.android.net

import dev.agentmesh.gen.agentmesh.term.v1.TermFrame
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.channels.ReceiveChannel
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.Request
import okio.ByteString
import okio.ByteString.Companion.toByteString

/**
 * A terminal stream over OkHttp's WebSocket: the host's `/v1/term/<agent>?ticket=…` (docs/apps.md, "Terminals get
 * their own binary stream"), each message one binary `TermFrame`. It rides the host's client, so the route's
 * pinning and the `Authorization` header apply; OkHttp sends no `Origin`, which the host refuses.
 */
class OkHttpTerminalSocket(connection: Connection, path: String) : TerminalSocket {
    private val channel = Channel<TermEvent>(Channel.UNLIMITED)
    override val events: ReceiveChannel<TermEvent> get() = channel

    private val socket: WebSocket = connection.http.newWebSocket(
        Request.Builder().url(connection.baseUrl + path).build(),
        object : WebSocketListener() {
            override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
                try {
                    channel.trySend(TermEvent.Frame(TermFrame.parseFrom(bytes.toByteArray())))
                } catch (e: Exception) {
                    channel.trySend(TermEvent.Failed(e))
                    webSocket.cancel()
                    channel.close()
                }
            }

            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                webSocket.close(code, reason)
                channel.trySend(TermEvent.Closed(code, reason))
                channel.close()
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                channel.trySend(TermEvent.Failed(if (response != null && response.code == 401) ApiException(com.connectrpc.Code.UNAUTHENTICATED, "the host refused the terminal ticket", t) else t))
                channel.close()
            }
        },
    )

    override fun send(frame: TermFrame): Boolean = socket.send(frame.toByteArray().toByteString())

    override fun close() {
        socket.close(1000, "done")
        channel.close()
    }
}
