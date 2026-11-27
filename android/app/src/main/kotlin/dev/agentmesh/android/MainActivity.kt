package dev.agentmesh.android

import android.Manifest
import android.content.Intent
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.CompositionLocalProvider
import dev.agentmesh.android.notify.ConnectionService
import dev.agentmesh.android.notify.DeepLink
import dev.agentmesh.android.notify.EXTRA_AGENT
import dev.agentmesh.android.notify.EXTRA_HOST
import dev.agentmesh.android.notify.EXTRA_TAB
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AppNav
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.rememberTheme

class MainActivity : ComponentActivity() {
    private val container get() = (application as MeshApp).container

    private val askNotifications = registerForActivityResult(ActivityResultContracts.RequestPermission()) { }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        val container = container
        setContent {
            CompositionLocalProvider(LocalContainer provides container) {
                AgentMeshTheme(rememberTheme(container)) { AppNav() }
            }
        }
        handle(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handle(intent)
    }

    override fun onStart() {
        super.onStart()
        val c = container
        // Notifications are why the app stays connected: ask once, when there is a host to notify about.
        if (Build.VERSION.SDK_INT >= 33 && !c.prefs.askedForNotifications && c.hosts.hosts.value.isNotEmpty() && !c.notifier.allowed()) {
            c.prefs.askedForNotifications = true
            askNotifications.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
        // A foreground service may only be started from the foreground.
        if (c.prefs.background.value && c.hosts.hosts.value.isNotEmpty()) ConnectionService.start(this)
    }

    private fun handle(intent: Intent?) {
        // A pairing link opened from a camera app, a browser or a message: the pairing screen shows it for the user to confirm.
        val data = intent?.data
        if (intent?.action == Intent.ACTION_VIEW && data != null && data.scheme.equals("agentmesh", ignoreCase = true) && data.host.equals("pair", ignoreCase = true) && dev.agentmesh.android.net.PairingLink.find(data.toString()) != null) {
            container.pairLink.value = data.toString()
            intent.data = null
            return
        }
        val host = intent?.getStringExtra(EXTRA_HOST) ?: return
        val agent = intent.getStringExtra(EXTRA_AGENT) ?: return
        val tab = runCatching { Tab.valueOf(intent.getStringExtra(EXTRA_TAB).orEmpty()) }.getOrDefault(Tab.Chat)
        container.deepLink.value = DeepLink(host, agent, tab)
        intent.removeExtra(EXTRA_AGENT)
    }
}
