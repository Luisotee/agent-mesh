package dev.agentmesh.android.notify

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import dev.agentmesh.android.MainActivity
import dev.agentmesh.android.R

/** Where a notification's tap (or a link) goes: an agent's screen on a host, on one of its tabs. */
data class DeepLink(val hostId: String, val agentId: String, val tab: Tab)

const val EXTRA_HOST = "host"
const val EXTRA_AGENT = "agent"
const val EXTRA_TAB = "tab"
const val ACTION_APPROVE = "dev.agentmesh.android.APPROVE"
const val ACTION_REPLY = "dev.agentmesh.android.REPLY"
const val KEY_REPLY = "reply"

/**
 * Posts what [noticeFor] found. One notification per agent (a later one replaces the earlier), channels per
 * urgency, a tap that opens the agent, an Approve action on a plan and an inline Reply on a question, so the
 * common answers need no trip into the app.
 */
class Notifier(private val context: Context) {
    private val manager = NotificationManagerCompat.from(context)

    init {
        val sys = context.getSystemService(NotificationManager::class.java)
        sys.createNotificationChannel(NotificationChannel(CH_WAITING, "Waiting on you", NotificationManager.IMPORTANCE_HIGH).apply {
            description = "Plans to approve, questions to answer, agents stopped at a prompt"
        })
        sys.createNotificationChannel(NotificationChannel(CH_REPORTS, "Reports", NotificationManager.IMPORTANCE_DEFAULT).apply {
            description = "Agents that finished, failed or died"
        })
        sys.createNotificationChannel(NotificationChannel(CH_CONNECTION, "Connection", NotificationManager.IMPORTANCE_LOW).apply {
            description = "Shown while the app keeps your hosts connected in the background"
        })
    }

    fun allowed(): Boolean =
        Build.VERSION.SDK_INT < 33 || context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED

    private fun id(hostId: String, agentId: String) = "$hostId:$agentId".hashCode()
    private fun tag(hostId: String) = "agent-mesh:$hostId"

    fun post(hostId: String, hostName: String?, n: Notice) {
        if (!allowed()) return
        val nid = id(hostId, n.agentId)
        val waiting = n.kind == NoticeKind.Plan || n.kind == NoticeKind.Question || n.kind == NoticeKind.Input
        val open = PendingIntent.getActivity(
            context, nid, Intent(context, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP)
                .putExtra(EXTRA_HOST, hostId).putExtra(EXTRA_AGENT, n.agentId).putExtra(EXTRA_TAB, n.tab.name),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val b = NotificationCompat.Builder(context, if (waiting) CH_WAITING else CH_REPORTS)
            .setSmallIcon(R.drawable.ic_stat_mesh)
            .setContentTitle(n.title)
            .setContentText(n.body)
            .setStyle(NotificationCompat.BigTextStyle().bigText(n.body))
            .setContentIntent(open)
            .setAutoCancel(true)
            .setOnlyAlertOnce(false)
            .setCategory(if (waiting) Notification.CATEGORY_MESSAGE else Notification.CATEGORY_STATUS)
        if (hostName != null) b.setSubText(hostName)
        when (n.kind) {
            NoticeKind.Plan -> b.addAction(0, "Approve", action(nid, ACTION_APPROVE, hostId, n.agentId, mutable = false))
            NoticeKind.Question -> {
                val reply = androidx.core.app.RemoteInput.Builder(KEY_REPLY).setLabel("Your answer").build()
                b.addAction(NotificationCompat.Action.Builder(0, "Reply", action(nid, ACTION_REPLY, hostId, n.agentId, mutable = true)).addRemoteInput(reply).build())
            }
            else -> {}
        }
        manager.notify(tag(hostId), nid, b.build())
    }

    private fun action(nid: Int, action: String, hostId: String, agentId: String, mutable: Boolean): PendingIntent =
        PendingIntent.getBroadcast(
            context, nid xor action.hashCode(),
            Intent(context, NoticeActionReceiver::class.java).setAction(action).putExtra(EXTRA_HOST, hostId).putExtra(EXTRA_AGENT, agentId),
            PendingIntent.FLAG_UPDATE_CURRENT or (if (mutable && Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else PendingIntent.FLAG_IMMUTABLE),
        )

    fun cancel(hostId: String, agentId: String) = manager.cancel(tag(hostId), id(hostId, agentId))

    /** Replaces an agent's notification with the outcome of its action (an error the user should see). */
    fun showError(hostId: String, agentId: String, title: String, text: String) {
        if (!allowed()) return
        manager.notify(
            tag(hostId), id(hostId, agentId),
            NotificationCompat.Builder(context, CH_WAITING).setSmallIcon(R.drawable.ic_stat_mesh).setContentTitle(title).setContentText(text)
                .setStyle(NotificationCompat.BigTextStyle().bigText(text)).setAutoCancel(true).build(),
        )
    }

    companion object {
        const val CH_WAITING = "waiting"
        const val CH_REPORTS = "reports"
        const val CH_CONNECTION = "connection"
    }
}

/** The text typed into a notification's Reply, if any. */
fun replyText(intent: Intent): String? = androidx.core.app.RemoteInput.getResultsFromIntent(intent)?.getCharSequence(KEY_REPLY)?.toString()
