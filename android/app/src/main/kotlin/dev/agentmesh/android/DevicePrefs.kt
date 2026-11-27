package dev.agentmesh.android

import android.content.Context
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/** This device's own preferences that aren't the theme: none of them is on a host. */
class DevicePrefs(context: Context) {
    private val prefs = context.getSharedPreferences("prefs", Context.MODE_PRIVATE)

    // Off unless the user turned it on: the foreground service it starts makes Android show a permanent notification, and
    // UnifiedPush (below) is how notifications arrive with the app closed. Earlier versions had it on by default and
    // stored nothing until the switch was touched, so a stored value is a choice and is kept; no stored value is off.
    private val _background = MutableStateFlow(prefs.getBoolean(BACKGROUND, false))

    /** Keep the hosts connected while the app is in the background (a foreground service with a permanent notification), so notifications arrive. */
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
