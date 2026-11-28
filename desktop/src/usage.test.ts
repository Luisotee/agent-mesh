import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { AgyUsageRowSchema, ClaudeUsageSchema, ClaudeUsageWindowSchema, GetUsageResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { GetCostResponseSchema } from './gen/agentmesh/v1/report_pb'
import { costView, quotaSections, resetText, tokens, usageView } from './usage'

const NOW = new Date('2026-11-26T12:00:00Z')

describe('usageView', () => {
  const u = create(GetUsageResponseSchema, {
    quotaStop: 90,
    minQuota: 10,
    claude: {
      source: 'cache',
      atReserve: ['weekly window'],
      windows: [
        { key: 'five_hour', label: 'five-hour window', used: 42, resetsAt: timestampFromMs(NOW.getTime() + 130 * 60_000) },
        { key: 'seven_day', label: 'weekly window', used: 93.4 },
      ],
    },
    agyRows: [
      { group: 'Gemini', label: 'Neon', left: 60, resets: '3h' },
      { group: 'Gemini', label: 'Hydrogen', left: 10, resets: '1h', atReserve: true },
    ],
    agyTiers: [
      { tier: 'strong', model: 'neon', left: 60, hasLeft: true, notes: ['n1'] },
      { tier: 'mid', model: '' },
    ],
  })

  it('marks the windows and agy rows the host says are at the reserve', () => {
    const v = usageView(u)
    expect(v.claude.map((w) => w.atReserve)).toEqual([false, true])
    expect(v.agy.map((r) => r.atReserve)).toEqual([false, true])
    expect(v.warnings).toEqual(['Claude weekly window', 'agy Gemini Hydrogen'])
    expect(v.claudeSource).toBe('cache')
  })

  it('takes the lines from the host: a weekly row and a window each have their own reserve, and the reviews-only mark follows', () => {
    const v = usageView(
      create(GetUsageResponseSchema, {
        splitReserves: true,
        quotaStop: 85,
        quotaStopWeekly: 95,
        reviewsOnlyClaude: 70,
        reviewsOnlyAgy: 100,
        claude: {
          windows: [
            { key: 'five_hour', label: 'five-hour window', used: 80, reserve: 85 },
            { key: 'seven_day', label: 'weekly window', used: 80, reserve: 95 },
          ],
          reviewsOnly: ['five-hour window', 'weekly window'],
        },
        agyRows: [
          { group: 'Gemini', label: 'Five Hour Limit Remaining', left: 50 },
          { group: 'Gemini', label: 'Weekly Limit Remaining', left: 3, atReserve: true },
          { group: 'Gemini', label: 'Other', left: 20, reviewsOnly: true },
        ],
      }),
    )
    expect([v.reserve, v.reserveWeekly, v.reviewsOnly]).toEqual([85, 95, { claude: 70, agy: 100 }])
    expect(v.claude.map((w) => [w.reserve, w.reviewsOnly])).toEqual([[85, true], [95, true]])
    expect(v.agy.map((r) => [r.reserve, r.atReserve, r.reviewsOnly])).toEqual([[85, false, false], [95, true, false], [85, false, true]])
    expect(v.warnings).toEqual(['agy Gemini Weekly Limit Remaining'])
    expect(v.reviewsOnlyWarnings).toEqual(['Claude five-hour window', 'Claude weekly window', 'agy Gemini Other'])
  })

  it('reads a host that has no weekly reserve yet as one reserve for every window', () => {
    const v = usageView(create(GetUsageResponseSchema, { quotaStop: 80, claude: { windows: [{ key: 'seven_day', label: 'weekly window', used: 10 }] } }))
    expect([v.reserveWeekly, v.claude[0]!.reserve, v.reviewsOnly]).toEqual([80, 80, { claude: 100, agy: 100 }])
  })

  it('reads agy rows of a host that predates the weekly reserve from min_quota, as before', () => {
    const v = usageView(create(GetUsageResponseSchema, { quotaStop: 90, minQuota: 10, agyRows: [{ group: 'g', label: 'a', left: 5 }, { group: 'g', label: 'b', left: 60 }] }))
    expect(v.agy.map((r) => r.atReserve)).toEqual([true, false])
    expect(usageView(create(GetUsageResponseSchema, { quotaStop: 100, minQuota: 0, agyRows: [{ group: 'g', label: 'a', left: 0 }] })).agy.map((r) => r.atReserve)).toEqual([false])
  })

  it('reads 0 as a setting once the host has the split reserves', () => {
    const v = usageView(create(GetUsageResponseSchema, { splitReserves: true, quotaStop: 90, quotaStopWeekly: 0, reviewsOnlyClaude: 0, reviewsOnlyAgy: 100 }))
    expect([v.reserveWeekly, v.reviewsOnly]).toEqual([0, { claude: 0, agy: 100 }])
  })

  it('keeps what a tier would start on, and a missing "left"', () => {
    const v = usageView(u)
    expect(v.tiers).toEqual([
      { tier: 'strong', model: 'neon', left: 60, group: '', label: '', notes: ['n1'] },
      { tier: 'mid', model: '', left: undefined, group: '', label: '', notes: [] },
    ])
  })

  it('has no windows and no warnings when the host has no reading', () => {
    const v = usageView(create(GetUsageResponseSchema, { quotaStop: 90 }))
    expect(v.claude).toEqual([])
    expect(v.warnings).toEqual([])
  })

  it('does not second-guess the host: a window at 100% it does not list (its reset has passed) is not at the reserve', () => {
    expect(usageView(create(GetUsageResponseSchema, { quotaStop: 90, minQuota: 10, claude: { windows: [{ key: 'a', label: 'a', used: 100 }] } })).warnings).toEqual([])
  })
})

describe('resetText', () => {
  it('says how long, within a day', () => {
    expect(resetText(new Date(NOW.getTime() + 130 * 60_000), NOW)).toBe('resets in 2 h 10 min')
    expect(resetText(new Date(NOW.getTime() + 25 * 60_000), NOW)).toBe('resets in 25 min')
    expect(resetText(new Date(NOW.getTime() - 1000), NOW)).toBe('resetting')
    expect(resetText(undefined, NOW)).toBe('reset time unknown')
  })
})

describe('costView', () => {
  const r = create(GetCostResponseSchema, {
    scope: 'since 7d',
    usd: 12.345,
    pricesAsOf: '2026-11',
    totalTokens: 3_400_000n,
    totalRead: 80_000_000n,
    clis: [
      { cli: 'claude', calls: 120n, input: 2_000_000n, w1: 500_000n, w5: 100_000n, output: 790_000n, total: 3_390_000n, read: 79_000_000n, basis: 'exact: the API usage' },
      { cli: 'opencode', calls: 5n, input: 8000n, output: 2000n, total: 10_000n, read: 1_000_000n, usd: 0.05, basis: 'exact as the provider reports it' },
    ],
    kinds: [{ kind: 'claude', sessions: 4n, calls: 120n, usd: 12.3, share: 99.5, tokenShare: 70.5, total: 3_390_000n, input: 2_000_000n, w1: 500_000n, w5: 100_000n, output: 790_000n, read: 79_000_000n, usdPerCall: 0.1031, ctxPerCall: 150_000n, outPerCall: 800n }],
    byModel: [{ model: 'sonnet', phase: '', calls: 100n, usd: 8, input: 1000n, output: 2000n, total: 3000n, read: 5000n, ctxPerCall: 2_400_000n, outPerCall: 700n }],
    verdicts: [{ kind: 'agy', model: 'neon', tier: 'strong', reviews: 3n, findings: 5n, agreedPct: 60, hasAgreedPct: true, blocking: 1n }],
    reviews: { reviewedPrs: 3n, reviewersPerPr: 1.5, byCli: { agy: 2, claude: 1 }, prs: 2n, usd: 3, usdPerPr: 1.5 },
    resets: 2,
  })

  it('leads with tokens and writes the tables the CLI prints, with its number formats', () => {
    const v = costView(r)
    expect(v.tokens).toBe('3.4M tokens')
    expect(v.breakdown).toBe('input 2.0M, cache write 600k, output 792k; cache reads 80.0M apart')
    // The dollars are Claude's at API prices, beside the tokens.
    expect(v.usd).toBe('≈ $12.35')
    expect(v.scope).toBe('since 7d')
    expect(v.bases[0]).toEqual({ cli: 'claude', basis: 'exact: the API usage' })
    expect(v.tables.map((t) => t.title)).toEqual(['By CLI', 'By kind', 'By model and phase', "Review findings by the root's verdict"])
    expect(v.tables[0]!.headers).toEqual(['cli', 'calls', 'input', 'cache write', 'output', 'total', 'cache read', '≈ $'])
    expect(v.tables[0]!.rows[1]).toEqual(['opencode', '5', '8k', '0', '2k', '10k', '1.0M', '0.05'])
    expect(v.tables[1]!.rows[0]).toEqual(['claude', '4', '120', '2.0M', '600k', '790k', '3.4M', '79.0M', '70.5', '12.30', '150k', '800'])
    expect(v.tables[2]!.rows[0]).toEqual(['sonnet', '-', '100', '1k', '0', '2k', '3k', '5k', '8.00', '2.4M', '700'])
    // The dollars come after the tokens in a table.
    expect(v.tables[1]!.headers.indexOf('≈ $')).toBeGreaterThan(v.tables[1]!.headers.indexOf('total'))
    expect(v.tables[3]!.rows[0]).toEqual(['agy', 'neon', 'strong', '3', '5', '60%', '1'])
    expect(v.notes).toEqual(['Reviews: 3 PRs, 1.5 reviewers per PR (agy 2, claude 1)', 'Claude reviews: 2 PRs, ≈ $3.00 at API prices (≈ $1.50 per PR)', 'Root resets: 2'])
  })

  it('has no tables for an empty scope', () => {
    expect(costView(create(GetCostResponseSchema, { scope: 'since 24h' })).tables).toEqual([])
  })

  it('prefixes an agent with its run when the scope spans runs', () => {
    const rows = (scope: string) => costView(create(GetCostResponseSchema, { scope, agents: [{ run: 'r1', name: 'lead', kind: 'claude' }] })).tables[0]!.rows[0]![0]
    expect(rows('since 7d')).toBe('r1:lead')
    expect(rows('run r1')).toBe('lead')
  })
})

describe('tokens', () => {
  it('writes k and M', () => {
    expect(tokens(340_000n)).toBe('340k')
    expect(tokens(1_250_000)).toBe('1.3M')
    expect(tokens(300n)).toBe('300')
    expect(tokens(999_600)).toBe('1.0M') // not 1000k
    expect(tokens(999_400)).toBe('999k')
  })
})

describe("the footer's quota sections", () => {
  const win = (key: string, label: string, used: number) => create(ClaudeUsageWindowSchema, { key, label, used })
  const base = (over: MessageInitShape<typeof GetUsageResponseSchema> = {}) =>
    create(GetUsageResponseSchema, {
      quotaStop: 90,
      minQuota: 10,
      claude: create(ClaudeUsageSchema, { windows: [win('five_hour', 'five-hour window', 20), win('seven_day', 'seven-day window', 40), win('seven_day_opus', 'seven-day Opus', 10), win('seven_day_sonnet', 'seven-day Sonnet', 5)] }),
      agyRows: [create(AgyUsageRowSchema, { group: 'Gemini 4 Neon', label: 'Neon', left: 60, resets: '3h' }), create(AgyUsageRowSchema, { group: 'Gemini and Claude models', label: 'Shared', left: 80, resets: '2h' })],
      agyTiers: [{ tier: 'strong', model: 'Gemini 4 Neon', hasLeft: true, left: 60, notes: [] }],
      ...over,
    })

  it('has a section per provider, the short windows named 5h and 7d, and leaves out the minor ones', () => {
    const q = quotaSections(base())
    expect(q.reserve).toBe(90)
    expect(q.sections.map((s) => [s.vendor, s.name, s.lines.map((l) => [l.label, l.used])])).toEqual([
      ['claude', 'Claude', [['5h', 20], ['7d', 40]]],
      ['gemini', 'agy', [['Neon', 40]]],
    ])
  })

  it('brings a minor window back once it is kept for PR reviews', () => {
    const u = base({ claude: create(ClaudeUsageSchema, { windows: [win('five_hour', 'five-hour window', 20), win('seven_day_opus', 'seven-day Opus', 80)], reviewsOnly: ['seven-day Opus'] }) })
    expect(quotaSections(u).sections[0]!.lines.map((l) => [l.label, l.reviewsOnly])).toEqual([['5h', false], ['7d Opus', true]])
  })

  it('brings a minor window back once it is at the reserve', () => {
    const u = base({ claude: create(ClaudeUsageSchema, { windows: [win('five_hour', 'five-hour window', 20), win('seven_day_opus', 'seven-day Opus', 95)], atReserve: ['seven-day Opus'] }) })
    expect(quotaSections(u).sections[0]!.lines.map((l) => [l.label, l.atReserve])).toEqual([['5h', false], ['7d Opus', true]])
  })

  it('shows the fullest agy row when no tier says which one is used, and an unused pool at the reserve', () => {
    expect(quotaSections(base({ agyTiers: [] })).sections[1]!.lines.map((l) => l.label)).toEqual(['Neon'])
    const spent = base({ agyRows: [create(AgyUsageRowSchema, { group: 'Gemini and Claude models', label: 'Shared', left: 5, atReserve: true }), create(AgyUsageRowSchema, { group: 'Gemini 4 Neon', label: 'Neon', left: 60 })] })
    expect(quotaSections(spent).sections[1]!.lines.map((l) => [l.label, l.atReserve])).toEqual([['Shared', true], ['Neon', false]])
  })

  it('shows the fullest agy row when no tier names one (the real rows are "Gemini Models", the tiers\' models "flash")', () => {
    const real = base({
      claude: undefined,
      agyRows: [create(AgyUsageRowSchema, { group: 'Gemini Models', label: 'Weekly Limit Remaining', left: 70, resets: '2026-12-03T07:11:12Z' }), create(AgyUsageRowSchema, { group: 'Claude and GPT models', label: 'Five Hour Limit Remaining', left: 40 })],
      agyTiers: [{ tier: 'light', model: 'flash', hasLeft: true, left: 70, notes: [] }],
    })
    const q = quotaSections(real)
    expect(q.sections.map((s) => [s.name, s.lines.map((l) => [l.label, l.used])])).toEqual([['agy', [['Five Hour Limit Remaining', 60]]]])
  })

  it('shows the row the host matched to a tier, where the names say nothing', () => {
    const real = base({
      claude: undefined,
      agyRows: [create(AgyUsageRowSchema, { group: 'Gemini Models', label: 'Weekly Limit Remaining', left: 70 }), create(AgyUsageRowSchema, { group: 'Claude and GPT models', label: 'Five Hour Limit Remaining', left: 40 })],
      agyTiers: [{ tier: 'light', model: 'flash', hasLeft: true, left: 70, group: 'Gemini Models', label: 'Weekly Limit Remaining', notes: [] }],
    })
    expect(quotaSections(real).sections.map((s) => [s.name, s.lines.map((l) => [l.label, l.used])])).toEqual([['agy', [['Weekly Limit Remaining', 30]]]])
  })

  it('has no sections when the host has no reading', () => {
    expect(quotaSections(create(GetUsageResponseSchema)).sections).toEqual([])
  })
})
