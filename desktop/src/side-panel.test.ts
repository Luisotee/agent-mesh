import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { AgentPullRequestsSchema, PullRequestSchema } from './gen/agentmesh/v1/pr_pb'
import { AgyUsageRowSchema, ClaudeUsageSchema, ClaudeUsageWindowSchema, GetUsageResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentCostSchema, GetCostResponseSchema } from './gen/agentmesh/v1/report_pb'
import { AgentSchema } from './gen/agentmesh/v1/types_pb'
import { costOf, costRows, dollars, prRows, quotaRows, resetsIn } from './side-panel'

describe('cost', () => {
  const c = create(GetCostResponseSchema, { usd: 7.5, agents: [create(AgentCostSchema, { name: 'app-fix', usd: 2 }), create(AgentCostSchema, { name: 'app-lead', usd: 5.5 }), create(AgentCostSchema, { name: 'app-docs', usd: 0 })] })

  it('lists the agents dearest first, without those that cost nothing, with the host\'s total', () => {
    expect(costRows(c)).toEqual({ total: 7.5, rows: [{ name: 'app-lead', usd: 5.5 }, { name: 'app-fix', usd: 2 }] })
  })

  it('finds one agent\'s share by name', () => {
    expect(costOf(c, create(AgentSchema, { name: 'app-fix' }))).toBe(2)
    expect(costOf(c, create(AgentSchema, { name: 'nobody' }))).toBeUndefined()
    expect(costOf(undefined, create(AgentSchema, { name: 'app-fix' }))).toBeUndefined()
  })

  it('writes dollars with cents', () => expect(dollars(4.5)).toBe('$4.50'))
})

describe('prRows', () => {
  const agents = { a: create(AgentSchema, { id: 'a', name: 'app-fix', runId: 'r1' }), b: create(AgentSchema, { id: 'b', name: 'web-x', runId: 'r2' }) }
  const pr = (agentId: string, number: number, state: string) => create(PullRequestSchema, { agentId, number, state, url: `u${number}` })
  const prs = {
    a: create(AgentPullRequestsSchema, { agentId: 'a', pullRequests: [pr('a', 3, 'merged'), pr('a', 4, 'open'), pr('a', 2, 'open')] }),
    b: create(AgentPullRequestsSchema, { agentId: 'b', pullRequests: [pr('b', 9, 'open')] }),
    gone: create(AgentPullRequestsSchema, { agentId: 'gone', pullRequests: [pr('gone', 1, 'open')] }),
  }

  it('lists the PRs of the agents asked for, open first and newest first, and skips an agent the app lacks', () => {
    expect(prRows(prs, agents, (x) => x.runId === 'r1').map((r) => r.number)).toEqual([4, 2, 3])
    expect(prRows(prs, agents, () => true).map((r) => r.number)).toEqual([9, 4, 2, 3])
    expect(prRows(prs, agents, (x) => x.id === 'b')[0]).toMatchObject({ agentName: 'web-x', url: 'u9' })
  })
})

describe('quota', () => {
  it('shows Claude\'s windows with the host\'s at-reserve marks, and agy\'s fullest row', () => {
    const u = create(GetUsageResponseSchema, {
      quotaStop: 90,
      minQuota: 10,
      claude: create(ClaudeUsageSchema, { windows: [create(ClaudeUsageWindowSchema, { label: 'weekly window', used: 95, resetsAt: timestampFromMs(5000) }), create(ClaudeUsageWindowSchema, { label: 'five-hour window', used: 20 })], atReserve: ['weekly window'] }),
      agyRows: [create(AgyUsageRowSchema, { group: 'Gemini', label: 'Pro', left: 60 }), create(AgyUsageRowSchema, { group: 'Gemini', label: 'Flash', left: 8 })],
    })
    const q = quotaRows(u)
    expect(q.reserve).toBe(90)
    expect(q.rows.map((r) => [r.label, r.used, r.atReserve])).toEqual([['Claude weekly window', 95, true], ['Claude five-hour window', 20, false], ['agy Gemini Flash', 92, true]])
    expect(q.rows[0]!.resetsAt?.getTime()).toBe(5000)
  })

  it('has no rows when the host has no reading', () => {
    expect(quotaRows(create(GetUsageResponseSchema)).rows).toEqual([])
  })

  it('says when a window resets', () => {
    const now = new Date('2026-11-26T12:00:00Z')
    expect(resetsIn(new Date(now.getTime() + 30 * 60_000), now)).toBe('resets in 30 min')
    expect(resetsIn(new Date(now.getTime() + 130 * 60_000), now)).toBe('resets in 2 h 10 min')
    expect(resetsIn(new Date(now.getTime() - 1000), now)).toBe('resetting')
    expect(resetsIn(undefined, now)).toBe('')
  })
})
