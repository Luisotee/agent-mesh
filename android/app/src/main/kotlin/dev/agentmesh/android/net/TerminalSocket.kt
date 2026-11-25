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
                // Tell the reader first: answering the close must never turn it into a failure.
                channel.trySend(TermEvent.Closed(code, reason))
                channel.close()
                try { webSocket.close(echoCloseCode(code), reason.takeIf { echoCloseCode(code) == code }) } catch (e: IllegalArgumentException) { webSocket.cancel() }
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                channel.trySend(TermEvent.Failed(refusal(response, t)))
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

/** The code to answer a close with: the peer's own when a close frame may carry it (OkHttp refuses 1005, 1006 and the reserved ranges), else a normal close. */
fun echoCloseCode(code: Int): Int = if (code in 1000..4999 && code !in 1004..1006 && code !in 1012..2999) code else 1000

/** What a refused upgrade means: a ticket that expired (401, retried with a fresh one), or the host's answer to this request (403, 404). */
fun refusal(response: Response?, cause: Throwable): Throwable = when (response?.code) {
    401 -> ApiException(com.connectrpc.Code.UNAUTHENTICATED, "the host refused the terminal ticket", cause)
    403 -> ApiException(com.connectrpc.Code.PERMISSION_DENIED, "the host doesn't let this device open that terminal", cause)
    404 -> ApiException(com.connectrpc.Code.NOT_FOUND, "the host has no such terminal", cause)
    else -> cause
}
