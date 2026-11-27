package dev.agentmesh.android.notify

import dev.agentmesh.android.net.toJson
import dev.agentmesh.android.store.HostState
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull

/** What notifies by default until the host's settings arrive (the daemon's `DefaultNotifyEvents`). */
val DEFAULT_EVENTS = setOf(NoticeKind.Input, NoticeKind.Plan, NoticeKind.Question, NoticeKind.Report, NoticeKind.Failed, NoticeKind.Error)

/**
 * Whether [kind] notifies on this phone: the host's `app.notify.enabled` off silences everything; then this phone's own
 * list of kinds ([device], the settings screen's choice) if it chose one, else the host's `app.notify.events`.
 */
fun notifies(s: HostState, kind: NoticeKind, device: Set<String>? = null): Boolean {
    val enabled = (s.settings["app.notify.enabled"]?.value?.toJson() as? JsonPrimitive)?.booleanOrNull ?: true
    if (!enabled) return false
    if (!device.isNullOrEmpty()) return kind.key in device
    val list = s.settings["app.notify.events"]?.value?.toJson() as? JsonArray ?: return kind in DEFAULT_EVENTS
    return list.any { (it as? JsonPrimitive)?.content == kind.key }
}
