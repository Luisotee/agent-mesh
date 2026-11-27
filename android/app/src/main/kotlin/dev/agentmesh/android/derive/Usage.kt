package dev.agentmesh.android.derive

import dev.agentmesh.android.store.toMillis
import dev.agentmesh.gen.agentmesh.v1.GetCostResponse
import dev.agentmesh.gen.agentmesh.v1.GetUsageResponse
import java.util.Locale

/**
 * What `mesh usage` and `mesh cost` show, as rows a screen draws (the desktop's `usage.ts`, with its rules). Nothing
 * here asks the host for anything.
 */
class QuotaBar(
    val key: String,
    val label: String,
    /** Percent of the window used, 0 to 100. */
    val used: Double,
    /** At or past the reserve: the host starts nothing new on it (`quota_stop`). */
    val atReserve: Boolean,
    val resetsAtMs: Long?,
    /** Past the CLI's reviews-only line, under the reserve: the host starts only PR reviews on it. */
    val reviewsOnly: Boolean = false,
)

class AgyRow(val group: String, val label: String, val left: Int, val resets: String, val atReserve: Boolean, val reviewsOnly: Boolean = false)

class AgyTier(val tier: String, val model: String, val left: Int?, val notes: List<String>)

class UsageView(
    /** The percent of a five-hour quota used at which the host starts nothing new (`quota_stop`). */
    val reserve: Int,
    /** The same for a weekly quota (`quota_stop_weekly`). */
    val reserveWeekly: Int,
    /** Where Claude and agy take only PR reviews (`quota_reviews_only.*`); 100 is off. */
    val reviewsOnlyClaude: Int,
    val reviewsOnlyAgy: Int,
    /** Claude's windows; empty when the host has no reading. */
    val claude: List<QuotaBar>,
    val claudeSource: String,
    val claudeFetchedAtMs: Long?,
    val agy: List<AgyRow>,
    val tiers: List<AgyTier>,
    /** The windows and rows at the reserve, named, for a warning. */
    val warnings: List<String>,
    /** The windows and rows past the reviews-only line but under the reserve, named. */
    val reviewsOnlyWarnings: List<String>,
)

fun usageView(u: GetUsageResponse): UsageView {
    val reserve = u.quotaStop
    // A host that predates the weekly reserve says 0 for it: its one reserve held every window.
    val reserveWeekly = if (u.quotaStopWeekly != 0) u.quotaStopWeekly else reserve
    val claude = u.claude.windowsList.map { w ->
        // The host says which windows are at the reserve (a window whose reset has passed isn't, whatever it last read).
        QuotaBar(w.key, w.label, w.used, w.label in u.claude.atReserveList, if (w.hasResetsAt()) w.resetsAt.toMillis() else null, w.label in u.claude.reviewsOnlyList)
    }
    // agy reports what is left; the host says which rows are at their line (a weekly row has the weekly reserve). A host that
    // predates the weekly reserve sends no flags: its one reserve, from `min_quota`, is read as before.
    val agy = u.agyRowsList.map { AgyRow(it.group, it.label, it.left, it.resets, if (u.quotaStopWeekly == 0) reserve < 100 && it.left <= u.minQuota else it.atReserve, it.reviewsOnly) }
    val tiers = u.agyTiersList.map { AgyTier(it.tier, it.model, if (it.hasLeft) it.left else null, it.notesList) }
    val warnings = claude.filter { it.atReserve }.map { "Claude ${it.label}" } + agy.filter { it.atReserve }.map { "agy ${it.group} ${it.label}" }
    val reviewsOnlyWarnings = claude.filter { it.reviewsOnly }.map { "Claude ${it.label}" } + agy.filter { it.reviewsOnly }.map { "agy ${it.group} ${it.label}" }
    return UsageView(
        reserve, reserveWeekly, u.reviewsOnlyClaude.takeIf { it != 0 } ?: 100, u.reviewsOnlyAgy.takeIf { it != 0 } ?: 100,
        claude, u.claude.source, if (u.claude.hasFetchedAt()) u.claude.fetchedAt.toMillis() else null, agy, tiers, warnings, reviewsOnlyWarnings,
    )
}

/** One line of the drawer's quota footer. */
class QuotaLine(val label: String, val used: Double, val atReserve: Boolean, val resetsAtMs: Long?)

/** The tone of a quota line: ok below the reserve, warn from it on, over when used up. */
enum class QuotaTone { Ok, Warn, Over }

val QuotaLine.tone: QuotaTone get() = if (used >= 100) QuotaTone.Over else if (atReserve) QuotaTone.Warn else QuotaTone.Ok

/** The compact reading the drawer shows (the desktop's `quotaRows`): Claude's windows, and agy's fullest row; empty without a reading. */
fun quotaLines(v: UsageView): List<QuotaLine> {
    val rows = v.claude.map { QuotaLine("Claude ${it.label}", it.used, it.atReserve, it.resetsAtMs) }.toMutableList()
    v.agy.minByOrNull { it.left }?.let { rows.add(QuotaLine("agy ${it.group} ${it.label}".trim(), 100.0 - it.left, it.atReserve, null)) }
    return rows
}

/** When a window resets, as a span from [nowMs] ("resets in 2 h 10 min") within a day, else the day and time. */
fun resetText(atMs: Long?, nowMs: Long, dayAndTime: (Long) -> String): String {
    if (atMs == null) return "reset time unknown"
    val min = Math.round((atMs - nowMs) / 60_000.0)
    return when {
        min <= 0 -> "resetting"
        min < 60 -> "resets in $min min"
        min < 24 * 60 -> "resets in ${min / 60} h ${min % 60} min"
        else -> "resets ${dayAndTime(atMs)}"
    }
}

fun usd(v: Double): String = String.format(Locale.US, "$%.2f", v)

/** Token counts the way `mesh cost` writes them: 1.2M, 340k. */
fun tokens(n: Long): String {
    if (n < 1000) return n.toString()
    // The unit is chosen after rounding: 999_600 is 1.0M, not 1000k.
    return if (Math.round(n / 1e3) >= 1000) String.format(Locale.US, "%.1fM", n / 1e6) else "${Math.round(n / 1e3)}k"
}

/** One line of a cost table: the name, its total tokens first, how they split, and the dollars beside them. */
class CostRow(val name: String, val tokens: String, val detail: String, val usd: String)

class CostSection(val title: String, val rows: List<CostRow>)

class CostView(
    /** "run abc123", "since 7d" and the like, as the host names what it measured. */
    val scope: String,
    /** The main figure: input + cache writes + output over every CLI, as "12.3M tokens". */
    val tokens: String,
    /** What the total holds, and the cache reads kept apart from it. */
    val breakdown: String,
    /** What the Claude sessions would cost at API prices ("≈ $4.51"): context beside the tokens, not the measure. */
    val usd: String,
    val pricesAsOf: String,
    /** Per CLI, how far its counts can be trusted. */
    val bases: List<Pair<String, String>>,
    val sections: List<CostSection>,
    /** Summary lines under the tables: reviews and root resets. */
    val notes: List<String>,
)

private fun split(input: Long, write: Long, output: Long, read: Long) =
    "input ${tokens(input)} · cache write ${tokens(write)} · output ${tokens(output)} · cache read ${tokens(read)}"

private fun approx(v: Double) = "≈ ${usd(v)}"

/** `mesh cost`'s tables for a phone: a row per CLI, kind, model and phase, agent; tokens first, dollars beside. */
fun costView(r: GetCostResponse): CostView {
    val sections = ArrayList<CostSection>()
    if (r.clisCount > 0) sections.add(CostSection("By CLI", r.clisList.map { CostRow("${it.cli} · ${it.calls} calls", tokens(it.total), split(it.input, it.w1 + it.w5, it.output, it.read), approx(it.usd)) }))
    if (r.kindsCount > 0) sections.add(CostSection("By kind", r.kindsList.map {
        CostRow("${it.kind} · ${it.sessions} sessions, ${it.calls} calls", tokens(it.total), "${String.format(Locale.US, "%.1f", it.tokenShare)}% of tokens · ${split(it.input, it.w1 + it.w5, it.output, it.read)}", approx(it.usd))
    }))
    if (r.byModelCount > 0) sections.add(CostSection("By model and phase", r.byModelList.map {
        CostRow("${it.model} · ${it.phase.ifEmpty { "-" }} · ${it.calls} calls", tokens(it.total), split(it.input, it.w1 + it.w5, it.output, it.read), approx(it.usd))
    }))
    if (r.agentsCount > 0) sections.add(CostSection("By agent", r.agentsList.map {
        val name = if (it.run.isNotEmpty() && !r.scope.startsWith("run ")) "${it.run}:${it.name}" else it.name
        CostRow("$name · ${it.kind} · ${it.calls} calls", tokens(it.total), split(it.input, it.w1 + it.w5, it.output, it.read), "${approx(it.usd)} (plan ${String.format(Locale.US, "%.2f", it.planUsd)} / exec ${String.format(Locale.US, "%.2f", it.execUsd)})")
    }))
    val notes = ArrayList<String>()
    val rev = r.reviews
    if (rev.reviewedPrs > 0) {
        val by = rev.byCliMap.entries.sortedBy { it.key }.joinToString(", ") { "${it.key} ${trim(it.value)}" }
        notes.add("Reviews: ${rev.reviewedPrs} PRs, ${trim(rev.reviewersPerPr)} reviewers per PR ($by)")
    }
    if (rev.prs > 0) notes.add("Claude reviews: ${rev.prs} PRs, ≈ ${usd(rev.usd)} at API prices (≈ ${usd(rev.usdPerPr)} per PR)")
    if (r.resets > 0) notes.add("Root resets: ${r.resets}")
    return CostView(
        scope = r.scope,
        tokens = "${tokens(r.totalTokens)} tokens",
        breakdown = "input ${tokens(r.clisList.sumOf { it.input })}, cache write ${tokens(r.clisList.sumOf { it.w1 + it.w5 })}, output ${tokens(r.clisList.sumOf { it.output })}; cache reads ${tokens(r.totalRead)} apart",
        usd = "≈ ${usd(r.usd)}",
        pricesAsOf = r.pricesAsOf,
        bases = r.clisList.map { it.cli to it.basis },
        sections = sections,
        notes = notes,
    )
}

/** A number as `mesh cost` writes it: two decimals at most, no trailing zeros. */
private fun trim(v: Double): String = String.format(Locale.US, "%.2f", v).trimEnd('0').trimEnd('.')

/** The spans the cost view offers, besides one run: the host's `--since` and what the chip says. */
val COST_WINDOWS = listOf("24h" to "Last 24 hours", "7d" to "Last 7 days", "30d" to "Last 30 days")
