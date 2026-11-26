package dev.agentmesh.android.notify

import dev.agentmesh.android.MeshApp
import org.unifiedpush.android.connector.FailedReason
import org.unifiedpush.android.connector.PushService
import org.unifiedpush.android.connector.data.PushEndpoint
import org.unifiedpush.android.connector.data.PushMessage

/**
 * Where the distributor (ntfy) reaches the app, even with its process stopped: Android starts the process for it.
 * The instance is the host's id ([PushChannel]). A push the library couldn't decrypt with this device's keys
 * isn't from the host and is dropped.
 */
class MeshPushService : PushService() {
    private val container get() = (application as MeshApp).container

    override fun onNewEndpoint(endpoint: PushEndpoint, instance: String) {
        val keys = endpoint.pubKeySet ?: return container.pushRegistrar.failed(instance, "The distributor gave no encryption keys")
        container.pushRegistrar.onEndpoint(instance, SavedEndpoint(endpoint.url, keys.pubKey, keys.auth))
    }

    override fun onMessage(message: PushMessage, instance: String) {
        if (!message.decrypted) return
        PushPayload.parse(message.content)?.let { container.onPush(instance, it) }
    }

    override fun onRegistrationFailed(reason: FailedReason, instance: String) = container.pushRegistrar.failed(
        instance,
        when (reason) {
            FailedReason.NETWORK -> "The distributor couldn't reach its server"
            FailedReason.ACTION_REQUIRED -> "The distributor needs you: open it"
            FailedReason.VAPID_REQUIRED -> "The distributor asks for a VAPID key, which agent-mesh doesn't use"
            FailedReason.INTERNAL_ERROR -> "The distributor failed to register"
        },
    )

    override fun onUnregistered(instance: String) = container.pushRegistrar.forget(instance)
}
