package dev.agentmesh.android.net

import com.connectrpc.Code
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.store.Frame
import dev.agentmesh.gen.agentmesh.v1.Cursor
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeout
import kotlinx.coroutines.withTimeoutOrNull
import java.util.concurrent.atomic.AtomicInteger
import kotlin.math.min

/** What a connection feeds: the app's state, and its log. */
interface ConnectionSink {
    fun onState(state: ConnState)

    /** Each answer to `Hello` (the first and every heartbeat): the host's clock reading is in it. */
    fun onHello(hello: HelloResponse)
    fun onFrame(frame: Frame)

    /** Where to resume the event stream from; null starts from a snapshot. */
    fun cursor(): Cursor?
    fun log(line: String)
}

private const val MIN_SESSION_MS = 1_000L

private class StreamEnded(val reason: String) : Exception("the host ended the stream: ${reason.ifEmpty { "no reason given" }}")
private class StreamClosed : Exception("the event stream closed")

/**
 * One host's connection (docs/apps.md, "Protocols and lifecycle"):
 *  - ready only when `Hello` answers, not when a socket opens;
 *  - "online" is a heartbeat (`Hello` on a timer) that keeps answering; one that doesn't drops the connection;
 *  - the backoff resets only after the first heartbeat of a connection is acknowledged, so a host that accepts
 *    and then drops us keeps backing off;
 *  - every attempt has a generation: a late answer or a stream frame of an older one is ignored;
 *  - a refused token stops the retries; any other close, an `End` frame included, resumes from the cursor;
 *  - [nudge] (the app came to the foreground, a network came up) skips a backoff wait, or checks a ready
 *    connection at once, and wakes a stopped one.
 */
class HostConnection(
    private val scope: CoroutineScope,
    /** Builds the client for one attempt, so a changed token or route is picked up; may throw [RouteException]. */
    private val api: () -> MeshApi,
    private val sink: ConnectionSink,
    private val now: () -> Long = System::currentTimeMillis,
    private val random: () -> Double = Math::random,
    private val heartbeatMs: Long = 15_000,
    private val helloTimeoutMs: Long = 10_000,
    private val baseMs: Long = 500,
    private val maxMs: Long = 15_000,
) {
    private val generation = AtomicInteger()
    private val attempt = AtomicInteger()
    private val wake = Channel<Unit>(Channel.CONFLATED)
    private var job: Job? = null

    fun start() {
        if (job?.isActive == true) return
        job = scope.launch { loop() }
    }

    fun stop() {
        job?.cancel()
        job = null
        generation.incrementAndGet()
    }

    /** Drop the connection and start over now: the route or the token changed. */
    fun restart() {
        stop()
        attempt.set(0)
        wake.tryReceive()
        start()
    }

    /** Try again now (skip a backoff wait, wake a stopped connection), or check a ready one at once. */
    fun nudge() {
        wake.trySend(Unit)
    }

    private suspend fun loop() {
        while (true) {
            val gen = generation.incrementAndGet()
            if (attempt.get() == 0) sink.onState(ConnState.Connecting)
            try {
                session(gen)
            } catch (e: CancellationException) {
                if (e is TimeoutCancellationException) failed(gen, e) else throw e
            } catch (e: Throwable) {
                failed(gen, e)
            }
            if (generation.get() != gen + 1) continue
            // `failed` bumped the generation and said why; wait out the backoff or the stop. A nudge that came
            // while the attempt ran is stale: it must not skip this wait.
            wake.tryReceive()
            val wait = waitMs
            if (wait < 0) wake.receive() else withTimeoutOrNull(wait) { wake.receive() }
            if (wait < 0) attempt.set(0)
        }
    }

    private var waitMs = 0L
    private var readyAt = 0L

    private suspend fun session(gen: Int) = coroutineScope {
        val client = try {
            api()
        } catch (e: RouteException) {
            throw ConfigError(e.message ?: "this route can't be used")
        }
        val hello = withTimeout(helloTimeoutMs) { client.hello() }
        if (gen != generation.get()) return@coroutineScope
        if (hello.protocolVersion != PROTOCOL_VERSION) {
            throw Incompatible("the host speaks protocol ${hello.protocolVersion}, this app ${PROTOCOL_VERSION}: update the older side")
        }
        sink.onHello(hello)
        val daemon = hello.daemonId
        readyAt = now()
        sink.log("connected to ${hello.daemonId} (instance ${hello.instanceId}, ${hello.version})")
        sink.onState(ConnState.Ready(hello))
        launch { heartbeat(gen, client, daemon) }
        client.subscribe(sink.cursor()).collect { msg ->
            if (gen != generation.get()) return@collect
            if (msg.frameCase == SubscribeEventsResponse.FrameCase.END) throw StreamEnded(msg.end.reason)
            sink.onFrame(msg)
        }
        throw StreamClosed()
    }

    private suspend fun heartbeat(gen: Int, client: MeshApi, daemon: String) {
        var first = true
        while (true) {
            withTimeoutOrNull(heartbeatMs) { wake.receive() }
            if (gen != generation.get()) return
            val hello = try {
                withTimeout(helloTimeoutMs) { client.hello() }
            } catch (e: TimeoutCancellationException) {
                throw ApiException(Code.DEADLINE_EXCEEDED, "no answer to the heartbeat", e)
            }
            sink.onHello(hello)
            // Another daemon behind the same address: what this stream carries is about the old one. Start over.
            if (hello.daemonId != daemon) throw ApiException(Code.UNAVAILABLE, "the daemon at this address changed", null)
            if (first && gen == generation.get()) {
                first = false
                attempt.set(0)
            }
        }
    }

    private class ConfigError(message: String) : Exception(message)
    private class Incompatible(message: String) : Exception(message)

    private fun failed(gen: Int, e: Throwable) {
        if (gen != generation.get()) return
        generation.incrementAndGet()
        val message = e.message ?: e.javaClass.simpleName
        when {
            e is ConfigError -> stopWith(ConnState.Reason.Config, message)
            e is Incompatible -> stopWith(ConnState.Reason.Incompatible, message)
            e is ApiException && e.refusedToken -> stopWith(ConnState.Reason.Auth, message)
            e is ApiException && e.code == Code.FAILED_PRECONDITION ->
                // From Hello this is a protocol mismatch ("upgrade the client").
                stopWith(if (Regex("protocol", RegexOption.IGNORE_CASE).containsMatchIn(message)) ConnState.Reason.Incompatible else ConnState.Reason.Config, message)
            else -> retryLater(message, expected = e is StreamEnded && now() - readyAt >= MIN_SESSION_MS)
        }
    }

    private fun stopWith(reason: ConnState.Reason, message: String) {
        sink.log("stopped (${reason.name.lowercase()}): $message")
        sink.onState(ConnState.Stopped(reason, message))
        waitMs = -1
    }

    private fun retryLater(message: String, expected: Boolean) {
        // An End frame after a session that lasted is the host shutting down on purpose: reconnect at once, with no
        // growing wait. One that comes at once, again and again, backs off like any other drop.
        val n = if (expected) 0 else attempt.get()
        val wait = if (expected) 0L else (min(maxMs.toDouble(), baseMs * Math.pow(2.0, n.toDouble())) * (0.5 + random() / 2)).toLong()
        if (!expected) attempt.incrementAndGet()
        sink.log(if (expected) "$message; reconnecting" else "lost the host ($message); retrying in ${"%.1f".format(wait / 1000.0)} s")
        sink.onState(ConnState.Reconnecting(attempt.get(), now() + wait, message))
        waitMs = wait
    }
}
