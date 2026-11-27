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

    private val _notifyEvents = MutableStateFlow(prefs.getStringSet(EVENTS, null)?.takeIf { it.isNotEmpty() })

    /**
     * The kinds of notice this phone chose to be told of (`input`, `plan`, …), for local notifications and for pushes (sent
     * to each host with the registration); null follows each host's own `app.notify.events`. A list of none is the same as no
     * choice: turning every kind off is the host's `app.notify.enabled`.
     */
    val notifyEvents: StateFlow<Set<String>?> get() = _notifyEvents

    fun setNotifyEvents(kinds: Set<String>?) {
        val v = kinds?.takeIf { it.isNotEmpty() }
        prefs.edit().apply { if (v == null) remove(EVENTS) else putStringSet(EVENTS, v) }.apply()
        _notifyEvents.value = v
    }

    /** Whether the notification permission was already asked for: it is asked once, then left to Settings. */
    var askedForNotifications: Boolean
        get() = prefs.getBoolean(ASKED, false)
        set(v) = prefs.edit().putBoolean(ASKED, v).apply()

    private companion object {
        const val BACKGROUND = "background"
        const val ASKED = "asked_notifications"
        const val PUSH = "push"
        const val EVENTS = "notify_events"
    }
}
