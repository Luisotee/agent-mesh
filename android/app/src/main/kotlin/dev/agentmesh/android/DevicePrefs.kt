package dev.agentmesh.android

import android.content.Context
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/** This device's own preferences that aren't the theme: none of them is on a host. */
class DevicePrefs(context: Context) {
    private val prefs = context.getSharedPreferences("prefs", Context.MODE_PRIVATE)

    private val _background = MutableStateFlow(prefs.getBoolean(BACKGROUND, true))

    /** Keep the hosts connected while the app is in the background (a foreground service), so notifications arrive. */
    val background: StateFlow<Boolean> get() = _background

    fun setBackground(on: Boolean) {
        prefs.edit().putBoolean(BACKGROUND, on).apply()
        _background.value = on
    }

    private val _push = MutableStateFlow(prefs.getBoolean(PUSH, false))

    /** Notifications through UnifiedPush (a distributor such as ntfy), which arrive with the app's process stopped. */
    val push: StateFlow<Boolean> get() = _push

    fun setPush(on: Boolean) {
        prefs.edit().putBoolean(PUSH, on).apply()
        _push.value = on
    }

    /** Whether the notification permission was already asked for: it is asked once, then left to Settings. */
    var askedForNotifications: Boolean
        get() = prefs.getBoolean(ASKED, false)
        set(v) = prefs.edit().putBoolean(ASKED, v).apply()

    private companion object {
        const val BACKGROUND = "background"
        const val ASKED = "asked_notifications"
        const val PUSH = "push"
    }
}
