package dev.agentmesh.android.notify

import android.app.Activity
import android.content.Context
import org.unifiedpush.android.connector.UnifiedPush
import org.unifiedpush.android.connector.keys.KeyManager

/**
 * Push (docs/apps.md, "Push"): UnifiedPush, with a distributor app on the phone (ntfy, on the user's own server or
 * ntfy.sh) that holds one connection for every app and hands each its pushes. The app registers one *instance*
 * per host (the instance is the host's id), so each host gets an endpoint and keys of its own; the endpoint
 * arrives in [MeshPushService.onNewEndpoint] and [PushRegistrar] gives it to the host. A seam so tests run
 * without a distributor.
 */
interface PushChannel {
    /** The UnifiedPush distributors installed (package names). */
    fun distributors(): List<String>

    /** The distributor in use, once one has acknowledged a registration; null before. */
    fun current(): String?

    /** A distributor's name to show. */
    fun label(distributor: String): String = distributor

    /**
     * Picks the distributor: the one in use, else the system's default, else asks the user (the library's own
     * dialog). [done] says whether one is set.
     */
    fun pick(activity: Activity, done: (Boolean) -> Unit)

    /** Registers the host's instance; the endpoint comes back through [MeshPushService]. Safe to repeat. */
    fun register(hostId: String, hostName: String)

    /** Drops the host's instance at the distributor (its endpoint stops working, and the host then forgets it). */
    fun unregister(hostId: String)
}

/**
 * The real one: the UnifiedPush connector library. [keys] replaces the library's own key manager (the Android
 * Keystore) in tests, which have no Keystore; null is the library's default.
 */
class UnifiedPushChannel(private val context: Context, private val keys: KeyManager? = null) : PushChannel {
    override fun distributors(): List<String> = UnifiedPush.getDistributors(context)

    override fun current(): String? = UnifiedPush.getAckDistributor(context)

    override fun label(distributor: String): String = runCatching {
        val pm = context.packageManager
        pm.getApplicationLabel(pm.getApplicationInfo(distributor, 0)).toString()
    }.getOrDefault(distributor)

    override fun pick(activity: Activity, done: (Boolean) -> Unit) = UnifiedPush.tryUseCurrentOrDefaultDistributor(activity, done)

    override fun register(hostId: String, hostName: String) {
        val note = "agent-mesh: $hostName"
        if (keys == null) UnifiedPush.register(context, instance = hostId, messageForDistributor = note)
        else UnifiedPush.register(context, hostId, note, null, keys)
    }

    // The library forgets the saved distributor when its last instance is unregistered, and then registers later
    // ones silently with no distributor: removing the only host would break push for the next one while the
    // switch stays on. The distributor is put back; turning push on again picks it anew anyway.
    override fun unregister(hostId: String) {
        val saved = UnifiedPush.getSavedDistributor(context)
        if (keys == null) UnifiedPush.unregister(context, hostId) else UnifiedPush.unregister(context, hostId, keys)
        if (saved != null && UnifiedPush.getSavedDistributor(context) == null) UnifiedPush.saveDistributor(context, saved)
    }
}
