import type { GetUsageResponse } from './gen/agentmesh/v1/mesh_pb'
import type { GetCostResponse } from './gen/agentmesh/v1/report_pb'
import type { Vendor } from './vendor'

/** What `mesh usage` and `mesh cost` show, as rows the screen draws. Nothing here asks the host for anything. */

export interface QuotaBar {
  key: string
  label: string
  /** Percent of the window used, 0 to 100. */
  used: number
  /** At or past the reserve: the host starts nothing new on it (`quota_stop`, `ReserveGuard` in the daemon). */
  atReserve: boolean
  /** The percent used at which this window's reserve is: `quota_stop` for the five-hour one, `quota_stop_weekly` for a weekly one. */
  reserve: number
  /** Past the CLI's reviews-only line, under the reserve: the host starts only PR reviews on it. */
  reviewsOnly: boolean
  resetsAt?: Date
}

export interface AgyRow {
  group: string
  label: string
  left: number
  resets: string
  atReserve: boolean
  /** The percent used at which this row's reserve is: `quota_stop` for a short row, `quota_stop_weekly` for a weekly one. */
  reserve: number
  reviewsOnly: boolean
}

export interface AgyTier {
  tier: string
  /** What the tier would start on now; empty when every model of it is at the reserve. */
  model: string
  left?: number
  /** The agy row the tier's model draws on, as the host matched it (empty from an older host, or when none matched). */
  group: string
  label: string
  notes: string[]
}

export interface UsageView {
  /** The percent of a five-hour quota used at which the host starts nothing new (`quota_stop`). */
  reserve: number
  /** The same for a weekly quota (`quota_stop_weekly`). */
  reserveWeekly: number
  /** Where Claude and agy take only PR reviews (`quota_reviews_only.*`); 100 is off. */
  reviewsOnly: { claude: number; agy: number }
  /** Claude's windows; empty when the host has no reading. */
  claude: QuotaBar[]
  claudeSource: string
  claudeFetchedAt?: Date
  agy: AgyRow[]
  tiers: AgyTier[]
  /** The windows and rows at the reserve, named, for a warning. */
  warnings: string[]
  /** The windows and rows past the reviews-only line but under the reserve, named. */
  reviewsOnlyWarnings: string[]
}

const date = (t?: { seconds: bigint }) => (t ? new Date(Number(t.seconds) * 1000) : undefined)

export function usageView(u: GetUsageResponse): UsageView {
  const reserve = u.quotaStop
  // A host that predates the weekly reserve sends no `splitReserves`: its one reserve held every window, and the new
  // fields are unset (0 is then not a setting).
  const split = u.splitReserves
  const reserveWeekly = split ? u.quotaStopWeekly : u.quotaStop
  const claude = (u.claude?.windows ?? []).map((w) => ({
    key: w.key,
    label: w.label,
    used: w.used,
    // The host says which windows are at the reserve (a window whose reset has passed isn't, whatever it last read).
    atReserve: (u.claude?.atReserve ?? []).includes(w.label),
    reserve: split ? w.reserve : w.key === 'five_hour' ? reserve : reserveWeekly,
    reviewsOnly: (u.claude?.reviewsOnly ?? []).includes(w.label),
    resetsAt: date(w.resetsAt),
  }))
  // agy reports what is left; the host says which rows are at their line (`agyRowHit`: a weekly row has the weekly reserve).
  const agy = u.agyRows.map((r) => ({
    group: r.group,
    label: r.label,
    left: r.left,
    resets: r.resets,
    // A host that predates the weekly reserve sends no flags: its one reserve, from `min_quota`, is read as before.
    atReserve: split ? r.atReserve : reserve < 100 && r.left <= u.minQuota,
    reserve: /week/i.test(r.label) ? reserveWeekly : reserve,
    reviewsOnly: r.reviewsOnly,
  }))
  const tiers = u.agyTiers.map((t) => ({ tier: t.tier, model: t.model, left: t.hasLeft ? t.left : undefined, group: t.group, label: t.label, notes: t.notes }))
  const warnings = [...claude.filter((w) => w.atReserve).map((w) => `Claude ${w.label}`), ...agy.filter((r) => r.atReserve).map((r) => `agy ${r.group} ${r.label}`)]
  const reviewsOnlyWarnings = [...claude.filter((w) => w.reviewsOnly).map((w) => `Claude ${w.label}`), ...agy.filter((r) => r.reviewsOnly).map((r) => `agy ${r.group} ${r.label}`)]
  return {
    reserve,
    reserveWeekly,
    reviewsOnly: split ? { claude: u.reviewsOnlyClaude, agy: u.reviewsOnlyAgy } : { claude: 100, agy: 100 },
    claude,
    claudeSource: u.claude?.source ?? '',
    claudeFetchedAt: date(u.claude?.fetchedAt),
    agy,
    tiers,
    warnings,
    reviewsOnlyWarnings,
  }
}

/** One bar of the footer's quota: a window of a provider. */
export interface QuotaLine {
  /** What the bar says: "5h", "7d", an agy row's name. */
  label: string
  /** The window in words, for its tip. */
  title: string
  /** Percent used, 0 to 100. */
  used: number
  atReserve: boolean
  /** The percent used at which this bar's reserve is (its own: five-hour and weekly differ). */
  reserve: number
  /** Past the reviews-only line: the host starts only PR reviews on it. */
  reviewsOnly: boolean
  resetsAt?: Date
  /** agy gives its reset as text ("1h"), not a time. */
  resetsIn?: string
}

/** A provider's bars, under its mark. */
export interface QuotaSection {
  vendor: Vendor
  name: string
  lines: QuotaLine[]
}

/** "five_hour" and "seven-day window" read "5h" and "7d"; the model windows keep their model ("7d Opus"). */
function windowLabel(key: string, label: string): string {
  const k = `${key} ${label}`.toLowerCase()
  const base = /five.?hour|\b5h\b/.test(k) ? '5h' : /seven.?day|weekly|\b7d\b/.test(k) ? '7d' : ''
  if (!base) return label
  const model = /opus/.test(k) ? 'Opus' : /sonnet/.test(k) ? 'Sonnet' : ''
  return model ? `${base} ${model}` : base
}

const minorWindow = (key: string, label: string) => /opus|sonnet/i.test(`${key} ${label}`)

/**
 * Whether a tier of the chain starts on the model an agy row measures: the row the host matched to the tier (`agy.QuotaApplies`),
 * else, from a host that does not say or a tier it matched to none, the names overlapping, either way round.
 */
function rowIsUsed(r: AgyRow, tiers: AgyTier[]): boolean {
  const names = [r.group, r.label].map((n) => n.trim().toLowerCase()).filter(Boolean)
  return tiers.some((t) => {
    if (t.group || t.label) return t.group === r.group && t.label === r.label
    const m = t.model.trim().toLowerCase()
    return m !== '' && names.some((n) => n.includes(m) || m.includes(n))
  })
}

/**
 * What the sidebar's footer shows of the quota, a section per provider: Claude's windows (the short ones, "5h" and "7d")
 * and the agy rows a tier of the chain starts on (the row the host matched to the tier, else the names compared here; when none matches, the fullest row stands for agy). The minor ones (Claude's Opus and Sonnet windows, an agy pool no tier
 * uses) are left out until one is at the reserve; Usage and cost has them all. With no reading, no sections.
 */
export function quotaSections(u: GetUsageResponse): { reserve: number; sections: QuotaSection[] } {
  const v = usageView(u)
  const sections: QuotaSection[] = []
  const claude: QuotaLine[] = v.claude
    .filter((w) => !minorWindow(w.key, w.label) || w.atReserve || w.reviewsOnly)
    .map((w) => ({ label: windowLabel(w.key, w.label), title: `Claude ${w.label}`, used: w.used, atReserve: w.atReserve, reserve: w.reserve, reviewsOnly: w.reviewsOnly, resetsAt: w.resetsAt }))
  if (claude.length > 0) sections.push({ vendor: 'claude', name: 'Claude', lines: claude })
  // With no tier to say what is used, the fullest row stands for agy; with tiers, the rows they start on.
  const named = v.tiers.some((t) => t.model) ? v.agy.filter((r) => r.atReserve || r.reviewsOnly || rowIsUsed(r, v.tiers)) : []
  const used = named.length > 0 ? named : [...v.agy].sort((x, y) => x.left - y.left).slice(0, 1)
  const chosen = used.sort((x, y) => x.left - y.left).slice(0, 3)
  // Two pools can share a label ("Weekly Limit Remaining"): the group then says which.
  const clash = (r: AgyRow) => chosen.filter((o) => (o.label || o.group) === (r.label || r.group)).length > 1
  const agy: QuotaLine[] = chosen.map((r) => ({ label: clash(r) ? `${r.group} ${r.label}`.trim() : r.label || r.group, title: `agy ${r.group} ${r.label}`.trim(), used: 100 - r.left, atReserve: r.atReserve, reserve: r.reserve, reviewsOnly: r.reviewsOnly, resetsIn: r.resets }))
  if (agy.length > 0) sections.push({ vendor: 'gemini', name: 'agy', lines: agy })
  return { reserve: v.reserve, sections }
}

/** When a window resets, as a span from `now` ("resets in 2 h 10 min") within a day, else the day and time. */
export function resetText(at: Date | undefined, now = new Date()): string {
  if (!at) return 'reset time unknown'
  const min = Math.round((at.getTime() - now.getTime()) / 60_000)
  if (min <= 0) return 'resetting'
  if (min < 60) return `resets in ${min} min`
  if (min < 24 * 60) return `resets in ${Math.floor(min / 60)} h ${min % 60} min`
  return `resets ${at.toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })}`
}

export const usd = (v: number) => `$${v.toFixed(2)}`

/** Token counts the way `mesh cost` writes them: 1.2M, 340k. */
export function tokens(n: bigint | number): string {
  const v = Number(n)
  if (v < 1000) return String(v)
  // The unit is chosen after rounding: 999_600 is 1.0M, not 1000k.
  return Math.round(v / 1e3) >= 1000 ? `${(v / 1e6).toFixed(1)}M` : `${Math.round(v / 1e3)}k`
}

export interface CostTable {
  title: string
  headers: string[]
  rows: string[][]
}

export interface CostView {
  /** "run abc123", "since 7d" and the like, as the host names what it measured. */
  scope: string
  /** The main figure: input + cache writes + output over every CLI, as "12.3M tokens". */
  tokens: string
  /** What the total holds, and the cache reads kept apart from it. */
  breakdown: string
  /** What the Claude sessions would cost at API prices ("≈ $4.51"): context beside the tokens, not the measure. */
  usd: string
  pricesAsOf: string
  /** Per CLI, how far its counts can be trusted. */
  bases: { cli: string; basis: string }[]
  tables: CostTable[]
  /** Summary lines under the tables: reviews and root resets. */
  notes: string[]
}

const phase = (p: string) => p || '-'

interface Counts {
  input: bigint
  w1: bigint
  w5: bigint
  output: bigint
  total: bigint
  read: bigint
}

/** The token columns every table opens with: input, cache write, output, the total, and the cache reads apart. */
const COUNT_HEADERS = ['input', 'cache write', 'output', 'total', 'cache read']
const counts = (c: Counts) => [tokens(c.input), tokens(c.w1 + c.w5), tokens(c.output), tokens(c.total), tokens(c.read)]

/** `mesh cost`'s tables: tokens first (CLI, kind, model and phase, agent), dollars as a trailing "≈ $" column. */
export function costView(r: GetCostResponse): CostView {
  const tables: CostTable[] = []
  if (r.clis.length)
    tables.push({
      title: 'By CLI',
      headers: ['cli', 'calls', ...COUNT_HEADERS, '≈ $'],
      rows: r.clis.map((c) => [c.cli, String(c.calls), ...counts(c), c.usd.toFixed(2)]),
    })
  if (r.kinds.length)
    tables.push({
      title: 'By kind',
      headers: ['kind', 'sessions', 'calls', ...COUNT_HEADERS, '% of tokens', '≈ $', 'ctx/call', 'out/call'],
      rows: r.kinds.map((k) => [k.kind, String(k.sessions), String(k.calls), ...counts(k), k.tokenShare.toFixed(1), k.usd.toFixed(2), tokens(k.ctxPerCall), String(k.outPerCall)]),
    })
  if (r.byModel.length)
    tables.push({
      title: 'By model and phase',
      headers: ['model', 'phase', 'calls', ...COUNT_HEADERS, '≈ $', 'ctx/call', 'out/call'],
      rows: r.byModel.map((m) => [m.model, phase(m.phase), String(m.calls), ...counts(m), m.usd.toFixed(2), tokens(m.ctxPerCall), String(m.outPerCall)]),
    })
  if (r.agents.length)
    tables.push({
      title: 'By agent',
      headers: ['agent', 'kind', 'calls', ...COUNT_HEADERS, '≈ $', 'plan/exec ≈ $'],
      rows: r.agents.map((a) => [a.run && !r.scope.startsWith('run ') ? `${a.run}:${a.name}` : a.name, a.kind, String(a.calls), ...counts(a), a.usd.toFixed(2), `${a.planUsd.toFixed(2)}/${a.execUsd.toFixed(2)}`]),
    })
  if (r.verdicts.length)
    tables.push({
      title: "Review findings by the root's verdict",
      headers: ['kind', 'model', 'tier', 'reviews', 'findings', 'agreed', 'blocking'],
      rows: r.verdicts.map((v) => [v.kind, v.model, v.tier, String(v.reviews), String(v.findings), v.hasAgreedPct ? `${v.agreedPct}%` : '', String(v.blocking)]),
    })
  const notes: string[] = []
  const rev = r.reviews
  if (rev && rev.reviewedPrs > 0) {
    const by = Object.entries(rev.byCli).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => `${k} ${+v.toFixed(2)}`)
    notes.push(`Reviews: ${rev.reviewedPrs} PRs, ${+rev.reviewersPerPr.toFixed(2)} reviewers per PR (${by.join(', ')})`)
  }
  if (rev && rev.prs > 0) notes.push(`Claude reviews: ${rev.prs} PRs, ≈ ${usd(rev.usd)} at API prices (≈ ${usd(rev.usdPerPr)} per PR)`)
  if (r.resets > 0) notes.push(`Root resets: ${r.resets}`)
  const sum = (f: (c: GetCostResponse['clis'][number]) => bigint) => r.clis.reduce((n, c) => n + f(c), 0n)
  return {
    scope: r.scope,
    tokens: `${tokens(r.totalTokens)} tokens`,
    breakdown: `input ${tokens(sum((c) => c.input))}, cache write ${tokens(sum((c) => c.w1 + c.w5))}, output ${tokens(sum((c) => c.output))}; cache reads ${tokens(r.totalRead)} apart`,
    usd: `≈ ${usd(r.usd)}`,
    pricesAsOf: r.pricesAsOf,
    bases: r.clis.map((c) => ({ cli: c.cli, basis: c.basis })),
    tables,
    notes,
  }
}

/** The spans the cost view offers, besides one run. */
export const WINDOWS = [
  { since: '24h', label: 'Last 24 hours' },
  { since: '7d', label: 'Last 7 days' },
  { since: '30d', label: 'Last 30 days' },
] as const
