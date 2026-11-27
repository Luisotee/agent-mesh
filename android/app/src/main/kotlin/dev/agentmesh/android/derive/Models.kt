package dev.agentmesh.android.derive

import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.CheckModelsResponse

/** The effort levels the host takes, lowest first (`daemon/internal/settings/schema.go`, `Efforts`). */
val EFFORTS = listOf("low", "medium", "high", "xhigh", "max")

/** "claude · sonnet · high": the CLI, model and effort an agent runs on, as far as the host says. */
fun runsOn(a: Agent): String = listOf(a.kind, a.model, a.effort).filter { it.isNotEmpty() }.joinToString(" · ")

/** What a reopen under way will run on, for "Switching to …" (the host fills the model and effort it resolved). */
fun pendingText(a: Agent): String {
    if (!a.hasPendingReopen()) return ""
    val r = a.pendingReopen
    if (r.model.isEmpty() && r.effort.isEmpty()) return ""
    val model = if (r.model.isNotEmpty() && r.model != a.model) r.model else ""
    val effort = if (r.effort.isNotEmpty() && r.effort != a.effort) "${r.effort} effort" else ""
    val to = listOf(model, effort).filter { it.isNotEmpty() }.joinToString(" at ")
    val why = if (r.why.isNotEmpty()) " (${r.why})" else ""
    return if (to.isNotEmpty()) "Switching to $to$why…" else "Reopening$why…"
}

/** The families the host lists for a CLI kind, from a `CheckModels` answer. */
fun familiesOf(r: CheckModelsResponse, kind: String): List<String> = r.kindsList.firstOrNull { it.kind == kind }?.listedList?.map { it.family }.orEmpty()

private val FALLBACK_TIERS = listOf("light", "mid", "strong", "top")

/** The tiers a host defines for a CLI (`tiers.<cli>.<tier>.*` settings), or the usual four before they are loaded. */
fun tiersOf(settingKeys: Collection<String>, kind: String): List<String> {
    val found = LinkedHashSet<String>()
    for (key in settingKeys) {
        val m = Regex("^tiers\\.([^.]+)\\.([^.]+)\\.").find(key) ?: continue
        if (m.groupValues[1] == kind) found.add(m.groupValues[2])
    }
    return if (found.isNotEmpty()) found.toList() else FALLBACK_TIERS
}
