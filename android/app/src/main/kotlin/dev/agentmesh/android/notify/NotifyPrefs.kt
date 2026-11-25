package dev.agentmesh.android.notify

import dev.agentmesh.android.net.toJson
import dev.agentmesh.android.store.HostState
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull

/** What notifies by default until the host's settings arrive (the daemon's `DefaultNotifyEvents`). */
val DEFAULT_EVENTS = setOf(NoticeKind.Input, NoticeKind.Plan, NoticeKind.Question, NoticeKind.Report, NoticeKind.Failed)

/** Whether this host's settings (`app.notify.enabled`, `app.notify.events`) let [kind] through. */
fun notifies(s: HostState, kind: NoticeKind): Boolean {
    val enabled = (s.settings["app.notify.enabled"]?.value?.toJson() as? JsonPrimitive)?.booleanOrNull ?: true
    if (!enabled) return false
    val list = s.settings["app.notify.events"]?.value?.toJson() as? JsonArray ?: return kind in DEFAULT_EVENTS
    return list.any { (it as? JsonPrimitive)?.content == kind.key }
}
