import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { GetUsageResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { GetCostResponseSchema } from './gen/agentmesh/v1/report_pb'
import { costView, resetText, tokens, usageView } from './usage'

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
      { group: 'Gemini', label: 'Hydrogen', left: 10, resets: '1h' },
    ],
    agyTiers: [
      { tier: 'strong', model: 'neon', left: 60, hasLeft: true, notes: ['n1'] },
      { tier: 'mid', model: '' },
    ],
  })

  it('marks the windows the host lists as at the reserve, and agy rows with min_quota or less left', () => {
    const v = usageView(u)
    expect(v.claude.map((w) => w.atReserve)).toEqual([false, true])
    expect(v.agy.map((r) => r.atReserve)).toEqual([false, true])
    expect(v.warnings).toEqual(['Claude weekly window', 'agy Gemini Hydrogen'])
    expect(v.claudeSource).toBe('cache')
  })

  it('keeps what a tier would start on, and a missing "left"', () => {
    const v = usageView(u)
    expect(v.tiers).toEqual([
      { tier: 'strong', model: 'neon', left: 60, notes: ['n1'] },
      { tier: 'mid', model: '', left: undefined, notes: [] },
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
    kinds: [{ kind: 'claude', sessions: 4n, calls: 120n, usd: 12.3, share: 99.5, usdPerCall: 0.1031, ctxPerCall: 150_000n, outPerCall: 800n }],
    byModel: [{ model: 'sonnet', phase: '', calls: 100n, usd: 8, ctxPerCall: 2_400_000n, outPerCall: 700n }],
    tokenKinds: [{ kind: 'opencode', sessions: 1n, calls: 5n, input: 10_000n, read: 0n, output: 2000n, usd: 0 }],
    verdicts: [{ kind: 'agy', model: 'neon', tier: 'strong', reviews: 3n, findings: 5n, agreedPct: 60, hasAgreedPct: true, blocking: 1n }],
    reviews: { reviewedPrs: 3n, reviewersPerPr: 1.5, byCli: { agy: 2, claude: 1 }, prs: 2n, usd: 3, usdPerPr: 1.5 },
    resets: 2,
  })

  it('writes the tables the CLI prints, with its number formats', () => {
    const v = costView(r)
    expect(v.usd).toBe('$12.35')
    expect(v.scope).toBe('since 7d')
    expect(v.tables.map((t) => t.title)).toEqual(['By CLI', 'By model and phase', 'agy and OpenCode (tokens)', "Review findings by the root's verdict"])
    expect(v.tables[0]!.rows[0]).toEqual(['claude', '4', '120', '12.30', '99.5', '0.103', '150k', '800'])
    expect(v.tables[1]!.rows[0]).toEqual(['sonnet', '-', '100', '8.00', '2.4M', '700'])
    // The token table has no $ column unless some row reports one.
    expect(v.tables[2]!.headers).not.toContain('$')
    expect(v.tables[3]!.rows[0]).toEqual(['agy', 'neon', 'strong', '3', '5', '60%', '1'])
    expect(v.notes).toEqual(['Reviews: 3 PRs, 1.5 reviewers per PR (agy 2, claude 1)', 'Claude reviews: 2 PRs, $3.00 ($1.50 per PR)', 'Root resets: 2'])
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
  })
})
