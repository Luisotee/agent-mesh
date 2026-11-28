import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { AgentRole, AgentSchema, AgentState, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { buildProjects, helpersLabel, leadRows, type SidebarHost } from './sidebar'

const agent = (id: string, o: Parameters<typeof create<typeof AgentSchema>>[1] = {}) => create(AgentSchema, { id, name: id, runId: 'r1', repo: '/p/app', role: AgentRole.WORKER, state: AgentState.IDLE, createdAt: timestampFromMs(1000), ...o })
const run = (id: string, o: Parameters<typeof create<typeof RunSchema>>[1] = {}) => create(RunSchema, { id, title: id, status: RunStatus.OPEN, ...o })
const by = <T extends { id: string }>(xs: T[]) => Object.fromEntries(xs.map((x) => [x.id, x]))
const host = (agents: ReturnType<typeof agent>[], runs = [run('r1')], o: Partial<SidebarHost> = {}): SidebarHost => ({ hostId: 'h', hostName: 'home', here: true, up: true, loading: false, runs: by(runs), agents: by(agents), waiting: new Set(), ...o })

describe('leadRows', () => {
  it('makes the root the lead, folds reviewers and askers into the agent they work for, and marks a worker elsewhere', () => {
    const [r] = leadRows(host([
      agent('lead', { role: AgentRole.ROOT }),
      agent('w', { parentId: 'lead' }),
      agent('rv', { role: AgentRole.REVIEWER, parentId: 'w' }),
      agent('ask', { role: AgentRole.ASKER, parentId: 'lead' }),
      agent('web', { parentId: 'lead', repo: '/p/web' }),
      agent('sub', { parentId: 'w' }),
    ]))
    expect(r!.lead?.id).toBe('lead')
    expect(r!.helpers.map((a) => a.id)).toEqual(['ask'])
    const w = r!.workers.find((x) => x.agent.id === 'w')!
    expect(w.helpers.map((a) => a.id)).toEqual(['rv'])
    expect(w.workers.map((x) => x.agent.id)).toEqual(['sub'])
    expect(r!.workers.find((x) => x.agent.id === 'web')!.elsewhere).toBe(true)
  })

  it('counts what waits on the user, leaves archived agents out, and folds a closed or finished run under Earlier', () => {
    const rows = leadRows(host(
      [
        agent('lead', { role: AgentRole.ROOT }),
        agent('w', { parentId: 'lead', state: AgentState.NEEDS_INPUT }),
        agent('gone', { parentId: 'lead', archived: true }),
        agent('l2', { role: AgentRole.ROOT, runId: 'r2', state: AgentState.EXITED }),
        agent('l3', { role: AgentRole.ROOT, runId: 'r3' }),
        agent('l4', { role: AgentRole.ROOT, runId: 'r4', archived: true }),
      ],
      [run('r1'), run('r2'), run('r3', { status: RunStatus.CLOSED }), run('r4')],
      { waiting: new Set(['lead']) },
    ))
    const r1 = rows.find((r) => r.run.id === 'r1')!
    expect(r1.needs).toBe(2)
    expect(r1.workers.map((w) => w.agent.id)).toEqual(['w'])
    expect(rows.find((r) => r.run.id === 'r2')!.earlier).toBe(true)
    expect(rows.find((r) => r.run.id === 'r3')!.earlier).toBe(true)
    // A run whose every agent is archived is gone from the sidebar.
    expect(rows.find((r) => r.run.id === 'r4')).toBeUndefined()
    // What waits on the user first.
    expect(rows[0]!.run.id).toBe('r1')
  })

  it('lifts the children of an archived agent into its place: an archived lead\'s live workers keep the run', () => {
    const rows = leadRows(host(
      [
        agent('lead', { role: AgentRole.ROOT, archived: true }),
        agent('w', { parentId: 'lead' }),
        agent('mid', { parentId: 'w', archived: true }),
        agent('sub', { parentId: 'mid', state: AgentState.NEEDS_INPUT }),
      ],
      [run('r1')],
    ))
    expect(rows).toHaveLength(1)
    expect(rows[0]!.lead?.id).toBe('w')
    expect(rows[0]!.workers.map((x) => x.agent.id)).toEqual(['sub'])
    expect(rows[0]!.needs).toBe(1)
  })
})

describe('leadRows hideEnded', () => {
  const agents = () => [
    agent('lead', { role: AgentRole.ROOT }),
    agent('live', { parentId: 'lead', state: AgentState.WORKING }),
    agent('idle', { parentId: 'lead' }),
    agent('retired', { parentId: 'lead', state: AgentState.RETIRED }),
    agent('lost', { parentId: 'lead', state: AgentState.EXITED }),
    agent('owes', { parentId: 'lead', state: AgentState.EXITED }),
    agent('mid', { parentId: 'lead', state: AgentState.RETIRED }),
    agent('kid', { parentId: 'mid', state: AgentState.WORKING }),
    agent('oldrv', { role: AgentRole.REVIEWER, parentId: 'live', state: AgentState.RETIRED }),
    agent('rv', { role: AgentRole.REVIEWER, parentId: 'live', state: AgentState.WORKING }),
  ]

  it('keeps live and idle agents, puts the ended ones in `ended`, lifts what an ended agent started into its place', () => {
    const [r] = leadRows(host(agents(), [run('r1')], { waiting: new Set(['owes']) }), { hideEnded: true })
    expect(r!.workers.map((w) => w.agent.id).sort()).toEqual(['idle', 'kid', 'live', 'owes'])
    expect(r!.ended.map((a) => a.id).sort()).toEqual(['lost', 'mid', 'oldrv', 'retired'])
    expect(r!.workers.find((w) => w.agent.id === 'live')!.helpers.map((a) => a.id)).toEqual(['rv'])
  })

  it('keeps an ended agent the UI flags as waiting on the user (a plan or question it left), where the others go', () => {
    const [r] = leadRows(host(agents(), [run('r1')], { waiting: new Set(['owes']) }), { hideEnded: true })
    expect(r!.workers.map((w) => w.agent.id)).toContain('owes')
    expect(r!.ended.map((a) => a.id)).not.toContain('owes')
    expect(r!.needs).toBe(1)
  })

  it('lists every agent as before unless asked, and counts the ended ones for the run being finished', () => {
    const [all] = leadRows(host(agents()))
    expect(all!.ended).toEqual([])
    expect(all!.workers.map((w) => w.agent.id)).toContain('retired')
    const [r] = leadRows(host([agent('lead', { role: AgentRole.ROOT, state: AgentState.EXITED }), agent('w', { parentId: 'lead', state: AgentState.RETIRED })]), { hideEnded: true })
    expect(r!.earlier).toBe(true)
    expect(r!.ended.map((a) => a.id)).toEqual(['w'])
  })

  it('names only the live read-only agents in the chip', () => {
    const [r] = leadRows(host(agents()), { hideEnded: true })
    expect(helpersLabel(r!.workers.find((w) => w.agent.id === 'live')!.helpers)).toBe('1 review')
  })
})

describe('buildProjects', () => {
  it('merges every host\'s projects: pinned first, then waiting, then the latest; the same folder on two hosts is two projects', () => {
    const a = host([
      agent('l1', { role: AgentRole.ROOT, repo: '/p/app', lastActivity: timestampFromMs(5000) }),
      agent('l2', { role: AgentRole.ROOT, runId: 'r2', repo: '/p/web', lastActivity: timestampFromMs(9000) }),
      agent('l3', { role: AgentRole.ROOT, runId: 'r3', repo: '/p/api', state: AgentState.NEEDS_INPUT }),
    ], [run('r1'), run('r2'), run('r3')])
    const b = host([agent('x', { role: AgentRole.ROOT, repo: '/p/app' })], [run('r1')], { hostId: 'g', hostName: 'vps', here: false })
    const p = buildProjects([a, b], { h: ['/p/docs'] })
    expect(p.map((x) => `${x.hostName}:${x.name}`)).toEqual(['home:docs', 'home:api', 'home:web', 'home:app', 'vps:app'])
    expect(p[0]).toMatchObject({ pinned: true, runs: [] })
    expect(p[1]!.needs).toBe(1)
  })

  it('lists a run with a worker in another repo under that project\'s "Also working here"', () => {
    const p = buildProjects([host([agent('lead', { role: AgentRole.ROOT }), agent('w', { parentId: 'lead', repo: '/p/web' })])], {})
    const web = p.find((x) => x.name === 'web')!
    expect(web.runs).toEqual([])
    expect(web.also.map((r) => r.run.id)).toEqual(['r1'])
    expect(p.find((x) => x.name === 'app')!.runs.map((r) => r.run.id)).toEqual(['r1'])
  })

  it('names read-only agents as one chip', () => {
    const rv = agent('a', { role: AgentRole.REVIEWER })
    expect(helpersLabel([rv, rv, agent('b', { role: AgentRole.ASKER })])).toBe('2 reviews, 1 asker')
    expect(helpersLabel([])).toBe('')
  })
})
