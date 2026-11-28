package dev.agentmesh.android.notify

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

/**
 * What a host's push carries once the UnifiedPush library has decrypted it (the host encrypts it for this device's
 * keys, RFC 8291, so the push server only sees ciphertext). The shape is the daemon's `pushPayload`
 * (`daemon/internal/daemon/push.go`): the notice the app would have posted from the live stream, already filtered
 * by the host's notify settings.
 */
@Serializable
data class PushPayload(
    val v: Int = 0,
    /** The daemon that sent it: a push for a host the app now knows as another daemon is dropped. */
    val daemon: String = "",
    /** The event it came from, so the live stream and the push post it once. 0 for a test push. */
    val seq: Long = 0,
    /** A [NoticeKind] key, or "test". */
    val kind: String = "",
    val title: String = "",
    val body: String = "",
    val agent: String = "",
    /** "terminal" opens the terminal; anything else (the host sends "messages") opens the chat. */
    val tab: String = "",
    /** When it happened, by the host's clock (ms). */
    val at: Long = 0,
) {
    val isTest: Boolean get() = kind == KIND_TEST

    /** The notice to post, or null for a kind this app doesn't know (a newer daemon's). */
    fun toNotice(): Notice? {
        val k = NoticeKind.entries.firstOrNull { it.key == kind } ?: return null
        if (agent.isEmpty()) return null
        val t = when (tab) {
            "terminal" -> Tab.Terminal
            else -> Tab.Chat
        }
        return Notice(k, title, body, agent, t, seq)
    }

    companion object {
        const val KIND_TEST = "test"
        private val json = Json { ignoreUnknownKeys = true }

        /** Reads a push's plaintext; null when it isn't one of ours (version 1). */
        fun parse(content: ByteArray): PushPayload? =
            runCatching { json.decodeFromString<PushPayload>(content.decodeToString()) }.getOrNull()?.takeIf { it.v == 1 }
    }
}
