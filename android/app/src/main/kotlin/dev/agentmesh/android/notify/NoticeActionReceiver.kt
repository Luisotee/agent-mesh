package dev.agentmesh.android.notify

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import dev.agentmesh.android.MeshApp
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.android.AlreadyAnswered
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeout

/**
 * A notification's Approve and Reply. The app may have been started just for this, so it waits (briefly) for the
 * host's connection; the answer is an ordinary action of the registry, with the usual idempotency metadata.
 */
class NoticeActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val host = intent.getStringExtra(EXTRA_HOST) ?: return
        val agent = intent.getStringExtra(EXTRA_AGENT) ?: return
        val reply = if (intent.action == ACTION_REPLY) replyText(intent)?.takeIf { it.isNotBlank() } ?: return else null
        val container = (context.applicationContext as MeshApp).container
        val pending = goAsync()
        container.scope.launch {
            try {
                // Everything, the host's runtime included, within the time a broadcast may take (about 10 s).
                withTimeout(8_000) {
                    val rt = container.runtimes.first { it.containsKey(host) }[host]!!
                    val name = rt.state.value.agents[agent]?.name ?: "the agent"
                    try {
                        rt.state.first { it.conn is ConnState.Ready }
                        if (reply == null) rt.approve(agent) else rt.answer(agent, reply)
                        container.notifier.cancel(host, agent)
                    } catch (e: AlreadyAnswered) {
                        container.notifier.cancel(host, agent)
                    } catch (e: TimeoutCancellationException) {
                        throw e
                    } catch (e: Exception) {
                        container.notifier.showError(host, agent, "Couldn't ${if (reply == null) "approve $name's plan" else "answer $name"}", e.message ?: "Open the app to try again.")
                    }
                }
            } catch (e: TimeoutCancellationException) {
                container.notifier.showError(host, agent, "Couldn't reach the host", "Open the app to ${if (reply == null) "approve the plan" else "answer"} there.")
            } finally {
                pending.finish()
            }
        }
    }
}
