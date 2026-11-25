package dev.agentmesh.android.net

import java.security.cert.CertificateException

/** The relay's refusal anywhere in a failure's causes (OkHttp and ConnectRPC wrap what a socket threw). */
fun relayRefusal(e: Throwable): RelayRefused? = generateSequence(e) { it.cause }.take(8).filterIsInstance<RelayRefused>().firstOrNull()

/** The pin's mismatch anywhere in a failure's causes ([PinnedTrust] throws it; TLS wraps it in an `SSLHandshakeException`). */
fun pinMismatch(e: Throwable): CertificateException? =
    generateSequence(e) { it.cause }.take(8).filterIsInstance<CertificateException>().firstOrNull { it.message?.contains("pinned") == true }

/** A failure retrying can't fix: the relay refused this device's client token, or the host's certificate isn't the pinned one. */
fun isFinalRouteFailure(e: Throwable): Boolean = relayRefusal(e)?.code == "auth" || pinMismatch(e) != null

/** What to tell the user about a failed connection: the relay's own word or the pin's mismatch when that was it, else the message. */
fun describeFailure(e: Throwable): String {
    relayRefusal(e)?.let { return it.message ?: "the relay refused the connection" }
    pinMismatch(e)?.let { return it.message!! }
    return e.message ?: e.javaClass.simpleName
}
