package dev.agentmesh.android

import android.content.Context
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/** This device's own preferences that aren't the theme: none of them is on a host. */
class DevicePrefs(context: Context) {
    private val prefs = context.getSharedPreferences("prefs", Context.MODE_PRIVATE)
    /** The unsent text of each agent's chat, in a file of its own so a draft never rewrites the other preferences. */
    private val drafts = context.getSharedPreferences("drafts", Context.MODE_PRIVATE)

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

    private val _pins = MutableStateFlow(readPins())

    /** The projects the user pinned to the top of the drawer, as repo paths by host id, in the order they were pinned. */
    val pins: StateFlow<Map<String, List<String>>> get() = _pins

    /** Pins a project; one already pinned stays pinned (and keeps its place in the order). */
    fun pin(hostId: String, repo: String) = setPins(hostId) { if (repo in it) it else it + repo }

    fun unpin(hostId: String, repo: String) = setPins(hostId) { it - repo }

    private fun setPins(hostId: String, change: (List<String>) -> List<String>) {
        val now = _pins.value[hostId].orEmpty()
        val next = change(now)
        if (next == now) return
        _pins.value = _pins.value.toMutableMap().also { if (next.isEmpty()) it.remove(hostId) else it[hostId] = next }
        val arr = org.json.JSONArray()
        for ((h, repos) in _pins.value) for (r in repos) arr.put(org.json.JSONArray().put(h).put(r))
        prefs.edit().putString(PINS, arr.toString()).apply()
    }

    private fun readPins(): Map<String, List<String>> = runCatching {
        val arr = org.json.JSONArray(prefs.getString(PINS, null) ?: return emptyMap())
        val out = LinkedHashMap<String, List<String>>()
        for (i in 0 until arr.length()) arr.getJSONArray(i).let { out[it.getString(0)] = out[it.getString(0)].orEmpty() + it.getString(1) }
        out
    }.getOrDefault(emptyMap())

    /** The text typed in an agent's chat and not sent, kept across navigation and restarts (device-local: no host sees it). */
    fun draft(hostId: String, agentId: String): String = drafts.getString("$hostId/$agentId", null).orEmpty()

    /** Keeps [text] as the agent's draft; blank forgets it (it was sent, or cleared). */
    fun setDraft(hostId: String, agentId: String, text: String) {
        val key = "$hostId/$agentId"
        if (text.isBlank()) { if (drafts.contains(key)) drafts.edit().remove(key).apply() }
        else if (drafts.getString(key, null) != text.take(MAX_DRAFT)) drafts.edit().putString(key, text.take(MAX_DRAFT)).apply()
    }

    /** The shells this phone opened (`hostId/shellId`): their terminal is sized to the phone's view, also after the app was restarted. A bounded set: the oldest are forgotten. */
    fun isPhoneShell(hostId: String, shellId: String): Boolean = "$hostId/$shellId" in prefs.getStringSet(PHONE_SHELLS, null).orEmpty()

    fun markPhoneShell(hostId: String, shellId: String) {
        val now = prefs.getStringSet(PHONE_SHELLS, null).orEmpty()
        val next = (now + "$hostId/$shellId").let { if (it.size > MAX_PHONE_SHELLS) it.drop(it.size - MAX_PHONE_SHELLS).toSet() else it }
        prefs.edit().putStringSet(PHONE_SHELLS, next).apply()
    }

    fun forgetPhoneShell(hostId: String, shellId: String) {
        val now = prefs.getStringSet(PHONE_SHELLS, null).orEmpty()
        if ("$hostId/$shellId" in now) prefs.edit().putStringSet(PHONE_SHELLS, now - "$hostId/$shellId").apply()
    }

    /** Forgets a removed host's pins, hides and drafts, so a host paired again starts clean. */
    fun forgetHost(hostId: String) {
        drafts.edit().also { e -> drafts.all.keys.filter { it.startsWith("$hostId/") }.forEach(e::remove) }.apply()
        prefs.getStringSet(PHONE_SHELLS, null)?.let { s -> val keep = s.filterTo(HashSet()) { !it.startsWith("$hostId/") }; if (keep.size != s.size) prefs.edit().putStringSet(PHONE_SHELLS, keep).apply() }
        if (hostId in _pins.value) {
            _pins.value = _pins.value - hostId
            val arr = org.json.JSONArray()
            for ((h, repos) in _pins.value) for (r in repos) arr.put(org.json.JSONArray().put(h).put(r))
            prefs.edit().putString(PINS, arr.toString()).apply()
        }
        val keep = _hidden.value.filterTo(HashSet()) { !it.startsWith("$hostId\n") }
        if (keep.size != _hidden.value.size) { prefs.edit().putStringSet(HIDDEN, keep).apply(); _hidden.value = keep }
    }

    private val _hidden = MutableStateFlow(prefs.getStringSet(HIDDEN, null).orEmpty().toSet())

    /** The projects hidden from the drawer (`hostId`, a newline, the repo path: `projectKey`); one comes back while something in it waits on the user. */
    val hidden: StateFlow<Set<String>> get() = _hidden

    fun hide(key: String) = setHidden(_hidden.value + key)

    fun unhide(key: String) = setHidden(_hidden.value - key)

    private fun setHidden(next: Set<String>) {
        if (next == _hidden.value) return
        prefs.edit().putStringSet(HIDDEN, next).apply()
        _hidden.value = next
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
        const val PINS = "pins"
        const val HIDDEN = "hidden_projects"
        const val PHONE_SHELLS = "phone_shells"
        const val MAX_PHONE_SHELLS = 60
        const val MAX_DRAFT = 50_000
    }
}
