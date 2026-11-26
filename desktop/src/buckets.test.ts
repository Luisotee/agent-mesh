import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { bucketOf, groupByProject, groupByState, type BucketContext } from './buckets'
import { AgentPullRequestsSchema, PullRequestSchema } from './gen/agentmesh/v1/pr_pb'
import { AgentRole, AgentSchema, AgentState, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'

const agent = (id: string, o: MessageInitShape<typeof AgentSchema> = {}) => create(AgentSchema, { id, name: id, runId: 'r1', repo: '/p/app', role: AgentRole.WORKER, state: AgentState.IDLE, ...o })
const prs = (agentId: string, ...p: { state: string; draft?: boolean }[]) =>
  create(AgentPullRequestsSchema, { agentId, pullRequests: p.map((x, i) => create(PullRequestSchema, { agentId, number: i + 1, url: `u${i}`, state: x.state, draft: x.draft ?? false })) })
const ctx = (o: Partial<BucketContext> = {}): BucketContext => ({ waiting: new Set(), pullRequests: {}, ...o })

describe('bucketOf', () => {
  it('puts what needs the user first: a prompt, or a plan or question that waits', () => {
    expect(bucketOf(agent('a', { state: AgentState.NEEDS_INPUT }), ctx())).toBe('needs')
    expect(bucketOf(agent('a', { state: AgentState.IDLE }), ctx({ waiting: new Set(['a']) }))).toBe('needs')
  })

  it('calls an error, a usage limit, a lost terminal and a nonzero exit failed, but not a stop by the user or a clean exit', () => {
    expect(bucketOf(agent('a', { state: AgentState.ERROR }), ctx())).toBe('failed')
    expect(bucketOf(agent('a', { state: AgentState.LIMITED }), ctx())).toBe('failed')
    expect(bucketOf(agent('a', { state: AgentState.EXITED, stateDetail: 'lost' }), ctx())).toBe('failed')
    expect(bucketOf(agent('a', { state: AgentState.EXITED, exitCode: 2 }), ctx())).toBe('failed')
    expect(bucketOf(agent('a', { state: AgentState.EXITED, stateDetail: 'killed', exitCode: -1 }), ctx())).toBe('done')
    expect(bucketOf(agent('a', { state: AgentState.EXITED, exitCode: 0 }), ctx())).toBe('done')
  })

  it('counts a start, a turn and a quiet agent as working', () => {
    for (const s of [AgentState.STARTING, AgentState.WORKING, AgentState.UNKNOWN]) expect(bucketOf(agent('a', { state: s }), ctx())).toBe('working')
  })

  it('makes a worker that stopped with an open PR ready to review, and one whose PRs are all merged done', () => {
    expect(bucketOf(agent('w'), ctx({ pullRequests: { w: prs('w', { state: 'open' }) } }))).toBe('review')
    expect(bucketOf(agent('w', { state: AgentState.RETIRED }), ctx({ pullRequests: { w: prs('w', { state: 'open' }) } }))).toBe('review')
    expect(bucketOf(agent('w'), ctx({ pullRequests: { w: prs('w', { state: 'merged' }, { state: 'closed' }) } }))).toBe('done')
    // A draft isn't ready; a worker still working with an open PR is working.
    expect(bucketOf(agent('w'), ctx({ pullRequests: { w: prs('w', { state: 'open', draft: true }) } }))).toBe('idle')
    expect(bucketOf(agent('w', { state: AgentState.WORKING }), ctx({ pullRequests: { w: prs('w', { state: 'open' }) } }))).toBe('working')
  })

  it('leaves a lead, a reviewer and a standing peer out of review, and tells idle from done by the process', () => {
    const open = { w: prs('w', { state: 'open' }) }
    expect(bucketOf(agent('w', { role: AgentRole.ROOT }), ctx({ pullRequests: open }))).toBe('idle')
    expect(bucketOf(agent('w', { role: AgentRole.REVIEWER, state: AgentState.RETIRED }), ctx({ pullRequests: open }))).toBe('done')
    expect(bucketOf(agent('w', { peer: true }), ctx({ pullRequests: open }))).toBe('idle')
    expect(bucketOf(agent('w', { state: AgentState.RETIRED }), ctx())).toBe('done')
  })
})

describe('grouping', () => {
  const runs = { r1: create(RunSchema, { id: 'r1' }), r2: create(RunSchema, { id: 'r2', status: RunStatus.CLOSED }) }
  const agents = {
    a: agent('a', { repo: '/p/app', state: AgentState.WORKING }),
    b: agent('b', { repo: '/p/app', state: AgentState.NEEDS_INPUT }),
    c: agent('c', { repo: '/p/web', state: AgentState.IDLE }),
    d: agent('d', { repo: '/p/web', runId: 'r2' }),
  }
  const base = (r: string) => r.split('/').pop() ?? r

  it('by project: a section per repo of the open runs, needing-you first, with counts', () => {
    const g = groupByProject(runs, agents, ctx(), base)
    expect(g.map((x) => x.label)).toEqual(['app', 'web'])
    expect(g[0]!.agents.map((x) => x.id)).toEqual(['b', 'a'])
    expect(g[0]!.counts).toMatchObject({ needs: 1, working: 1 })
    // d belongs to a closed run.
    expect(g[1]!.agents.map((x) => x.id)).toEqual(['c'])
  })

  it('by state: the buckets in order, none empty', () => {
    const g = groupByState(runs, agents, ctx())
    expect(g.map((x) => [x.label, x.agents.map((a) => a.id)])).toEqual([['Needs you', ['b']], ['Working', ['a']], ['Idle', ['c']]])
  })
})
