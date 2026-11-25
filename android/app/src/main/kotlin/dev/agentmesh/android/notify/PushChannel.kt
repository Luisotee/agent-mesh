package dev.agentmesh.android.notify

/**
 * The seam for push (docs/apps.md, "Push": UnifiedPush, with ntfy on the user's own server as the distributor), so
 * a notification can arrive with the app's process stopped and no foreground service running. Today the only
 * channel is the live event stream held by [ConnectionService]; a UnifiedPush receiver implements this and calls
 * [Notifier.post] with what the host sent, and the host's daemon sends to the user's own ntfy.
 */
interface PushChannel {
    /** Whether a distributor is registered and pushes can arrive. */
    val active: Boolean
    fun start()
    fun stop()
}

object NoPush : PushChannel {
    override val active = false
    override fun start() {}
    override fun stop() {}
}
