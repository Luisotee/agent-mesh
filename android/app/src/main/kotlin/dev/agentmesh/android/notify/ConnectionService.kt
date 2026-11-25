package dev.agentmesh.android.notify

import android.app.Notification
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.IBinder
import androidx.core.app.NotificationCompat
import dev.agentmesh.android.MainActivity
import dev.agentmesh.android.MeshApp
import dev.agentmesh.android.R
import dev.agentmesh.android.store.ConnState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch

/**
 * Holds the process (and so the hosts' event streams) alive while the app is in the background, which is what lets
 * a plan or a question notify the user. It only shows a quiet notification of its own; it does nothing else. It
 * stops itself when the setting is off or no host is saved.
 */
class ConnectionService : Service() {
    private val scope = CoroutineScope(Dispatchers.Main.immediate + Job())

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val container = (application as MeshApp).container
        startForeground(ID, build(this, "Connecting…"), ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        scope.launch {
            container.runtimes.flatMapLatest { rts ->
                if (rts.isEmpty()) kotlinx.coroutines.flow.flowOf(0 to 0)
                else combine(rts.values.map { it.state.map { s -> s.conn is ConnState.Ready } }) { ready -> ready.count { it } to ready.size }
            }.combine(container.prefs.background) { counts, on -> counts to on }.collect { (counts, on) ->
                if (!on || counts.second == 0) { stopSelf(); return@collect }
                val text = if (counts.first == counts.second) "Connected to ${plural(counts.second)}" else "${counts.first} of ${plural(counts.second)} connected"
                getSystemService(android.app.NotificationManager::class.java).notify(ID, build(this@ConnectionService, text))
            }
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        scope.coroutineContext[Job]?.cancel()
        super.onDestroy()
    }

    private fun plural(n: Int) = if (n == 1) "1 host" else "$n hosts"

    companion object {
        private const val ID = 1

        private fun build(context: Context, text: String): Notification =
            NotificationCompat.Builder(context, Notifier.CH_CONNECTION)
                .setSmallIcon(R.drawable.ic_stat_mesh).setContentTitle("agent-mesh").setContentText(text).setOngoing(true).setSilent(true)
                .setContentIntent(PendingIntent.getActivity(context, 0, Intent(context, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE))
                .build()

        /** Start it (from the foreground: Android refuses a background start). */
        fun start(context: Context) = context.startForegroundService(Intent(context, ConnectionService::class.java))
    }
}
