package dev.agentmesh.android.term

import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.TermEvent
import dev.agentmesh.android.net.TerminalSocket
import dev.agentmesh.android.net.describeFailure
import dev.agentmesh.android.net.isFinalRouteFailure
import dev.agentmesh.android.net.relayRefusal
import dev.agentmesh.gen.agentmesh.term.v1.Attach
import dev.agentmesh.gen.agentmesh.term.v1.AttachMode
import dev.agentmesh.gen.agentmesh.term.v1.TermFrame
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import kotlin.math.min

/** What a viewer's terminal is doing, for its status line. */
sealed interface TermState {
    /** The host's connection isn't ready yet, so there is nothing to attach to. */
    data object WaitingForHost : TermState
    data object Attaching : TermState
    data object Live : TermState
    data class Reconnecting(val attempt: Int, val why: String) : TermState

    /** The host closed the stream because the process ended, with no word on how: this is its last screen. */
    data object Ended : TermState

    /** The agent's process ended; this is its last screen. */
    data class Exited(val exitCode: Int, val signal: Int) : TermState

    /** Won't retry: the host refused, or there is no such terminal. */
    data class Failed(val message: String) : TermState
}

/** Close codes the host uses on a terminal stream (`daemon/internal/termhost/ws.go`). */
object CloseCode {
    const val EXITED = 4000
    const val SLOW = 4001
    const val SHUTDOWN = 4002
    const val BAD_REQUEST = 4003
}

/**
 * A read-only view of one agent's terminal (docs/apps.md, "Protocols and lifecycle", "Terminal protocol"):
 *  - the first attach asks for a repaint of the host's screen; every later one asks to continue from the last
 *    (epoch, byte offset), and the host falls back to a repaint when it can't;
 *  - the size is never ours to set: it attaches with size 0 ("unknown"), so a phone never shrinks the terminal the
 *    desktop is using, and draws the host's size;
 *  - a different epoch on live output means somebody resized: reattach with a repaint, which carries the new size;
 *  - a slow client (close 4001) reattaches by offset at once, any other drop after a growing wait, and each
 *    attach asks for a fresh one-time ticket;
 *  - it never sends input, so the host answers terminal queries and nothing the screen replies reaches the program.
 */
class TermClient(
    private val scope: CoroutineScope,
    private val agentId: String,
    private val api: suspend () -> MeshApi,
    private val screen: TermScreen,
    private val baseMs: Long = 500,
    private val maxMs: Long = 10_000,
    /** False while the host's connection isn't ready, which [api] waits for: the status line says so. */
    private val hostReady: () -> Boolean = { true },
) {
    private val _state = MutableStateFlow<TermState>(TermState.Attaching)
    val state: StateFlow<TermState> get() = _state

    private var job: Job? = null
    private var socket: TerminalSocket? = null

    // Where the stream is: the host's epoch and the offset of the next byte we expect.
    private var epoch = 0L
    private var offset = 0L
    private var haveScreen = false
    private var repaint = true

    fun start() {
        if (job?.isActive == true) return
        job = scope.launch { loop() }
    }

    /** Try again after a [TermState.Failed] (an agent whose terminal appears a moment later, an edited host). */
    fun retry() {
        if (_state.value !is TermState.Failed) return
        job = null
        _state.value = TermState.Attaching
        start()
    }

    fun stop() {
        job?.cancel()
        job = null
        socket?.close()
        socket = null
    }

    private suspend fun loop() {
        var attempt = 0
        var streak = 0
        while (true) {
            val outcome = try {
                session { attempt = 0; streak = 0 }
            } catch (e: kotlinx.coroutines.CancellationException) {
                throw e
            } catch (e: javax.net.ssl.SSLPeerUnverifiedException) {
                // The host's certificate isn't the pinned one: waiting won't change that.
                _state.value = TermState.Failed("The host's certificate is not the one pinned for it"); return
            } catch (e: Exception) {
                // The relay refused this device's client token, or the pin doesn't match (PinnedTrust's
                // CertificateException, wrapped in an SSLHandshakeException): waiting won't change either.
                if (isFinalRouteFailure(e)) {
                    _state.value = TermState.Failed(describeFailure(e) + if (relayRefusal(e) != null) ". Pair with the host again." else ""); return
                }
                if (e is ApiException && (e.refusedToken || e.code == com.connectrpc.Code.NOT_FOUND || e.code == com.connectrpc.Code.PERMISSION_DENIED || e.code == com.connectrpc.Code.FAILED_PRECONDITION)) {
                    _state.value = TermState.Failed(e.message ?: e.code.name.lowercase()); return
                }
                Outcome.Retry(describeFailure(e))
            }
            when (outcome) {
                is Outcome.Done -> return
                is Outcome.Failed -> { _state.value = TermState.Failed(outcome.message); return }
                // Reattach at once the first time; a host that keeps asking for it (slow, epoch, gap) without a
                // replay ever finishing gets the growing wait, so it is never a tight loop.
                Outcome.Now -> if (++streak > 1) delay(min(maxMs.toDouble(), baseMs * Math.pow(2.0, (streak - 2).toDouble())).toLong())
                is Outcome.Retry -> {
                    // A drop, not a request to reattach: what came before it isn't "consecutive immediate reattaches".
                    streak = 0
                    val wait = min(maxMs.toDouble(), baseMs * Math.pow(2.0, attempt.toDouble())).toLong()
                    attempt++
                    _state.value = TermState.Reconnecting(attempt, outcome.why)
                    delay(wait)
                }
            }
        }
    }

    private sealed interface Outcome {
        data object Done : Outcome
        data object Now : Outcome
        class Retry(val why: String) : Outcome
        class Failed(val message: String) : Outcome
    }

    private suspend fun session(onLive: () -> Unit): Outcome {
        if (!hostReady()) _state.value = TermState.WaitingForHost
        val client = api()
        if (_state.value == TermState.WaitingForHost) _state.value = TermState.Attaching
        val path = client.openTerminal(agentId)
        val sock = client.connectTerminal(path)
        socket = sock
        try {
            val attach = Attach.newBuilder()
                .setMode(if (repaint || !haveScreen) AttachMode.ATTACH_MODE_REPAINT else AttachMode.ATTACH_MODE_EXACT)
                .setEpoch(epoch).setOffset(offset) // size 0: ours to ask nothing of
            sock.send(TermFrame.newBuilder().setAttach(attach).build())
            for (ev in sock.events) {
                when (ev) {
                    is TermEvent.Frame -> handle(ev.frame, onLive)?.let { return it }
                    is TermEvent.Closed -> return when (ev.code) {
                        CloseCode.EXITED -> Outcome.Done.also { if (_state.value !is TermState.Exited) _state.value = TermState.Ended }
                        CloseCode.SLOW -> Outcome.Now
                        CloseCode.BAD_REQUEST -> Outcome.Failed(ev.reason.ifEmpty { "the host refused the stream" })
                        else -> Outcome.Retry(ev.reason.ifEmpty { "the host closed the stream" })
                    }
                    // A 401 on the upgrade can be a ticket that expired on a slow link: a retry with a fresh one, not the end.
                    // Any other refusal (403, 404) is the host's answer to this request, and the loop ends on it.
                    is TermEvent.Failed -> throw if (ev.cause is ApiException && ev.cause.code == com.connectrpc.Code.UNAUTHENTICATED) java.io.IOException(ev.cause.message ?: "the host refused the ticket") else ev.cause
                }
            }
            return Outcome.Retry("the stream ended")
        } finally {
            sock.close()
            if (socket === sock) socket = null
        }
    }

    /** One frame; non-null ends the session. */
    private suspend fun handle(f: TermFrame, onLive: () -> Unit): Outcome? {
        when (f.frameCase) {
            TermFrame.FrameCase.ATTACHED -> {
                val a = f.attached
                val fresh = a.mode == AttachMode.ATTACH_MODE_REPAINT || a.mode == AttachMode.ATTACH_MODE_TAIL || a.reanchored || !haveScreen
                if (fresh) screen.reset(a.cols.toInt(), a.rows.toInt())
                else if (screen.cols != a.cols.toInt() || screen.rows != a.rows.toInt()) screen.reset(a.cols.toInt(), a.rows.toInt())
                haveScreen = true
                repaint = false // the host answered: what we hold now is what it sent
                epoch = a.epoch
                offset = a.offset
                _state.value = TermState.Attaching
            }
            TermFrame.FrameCase.SCREEN -> {
                // The mode preamble and the screen as bytes a terminal renders; live output continues from here.
                val s = f.screen
                screen.write(s.data.toByteArray())
                epoch = s.epoch
                offset = s.offset
            }
            TermFrame.FrameCase.RESIZED -> {
                // Somebody resized: the output from here on is in the new epoch and size, which the screen takes in place. (A host
                // that doesn't send this frame is found out by the epoch of its next output, below.)
                val r = f.resized
                epoch = r.epoch
                screen.resize(r.cols.toInt(), r.rows.toInt())
            }
            TermFrame.FrameCase.OUTPUT -> {
                val o = f.output
                if (o.epoch != epoch) { repaint = true; return Outcome.Now } // somebody resized
                val data = o.data.toByteArray()
                when {
                    o.offset > offset -> { repaint = true; return Outcome.Now } // a gap: start over rather than draw garbage
                    o.offset + data.size <= offset -> {} // all of it is already on screen
                    else -> {
                        val skip = (offset - o.offset).toInt()
                        screen.write(if (skip == 0) data else data.copyOfRange(skip, data.size))
                        offset = o.offset + data.size
                    }
                }
                // Frames come in bursts: let the view draw between them.
                kotlinx.coroutines.yield()
            }
            TermFrame.FrameCase.REPLAY_DONE -> {
                _state.value = TermState.Live
                onLive()
            }
            TermFrame.FrameCase.EXITED -> {
                _state.value = TermState.Exited(f.exited.exitCode, f.exited.signal)
                return Outcome.Done
            }
            else -> {}
        }
        return null
    }

}
