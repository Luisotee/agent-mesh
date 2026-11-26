import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { buildTrees, knownRepos, messagesOf, repoName, sameRepo, statusSentence, joinWaiting } from './derive'
import { WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole, AgentSchema, AgentState, MessageSchema, MessageType, RunSchema, RunStatus, type Agent } from './gen/agentmesh/v1/types_pb'

const a = (id: string, o: Parameters<typeof create<typeof AgentSchema>>[1] = {}) => create(AgentSchema, { id, name: id, runId: 'r1', createdAt: timestampFromMs(1000), ...o })
const byId = <T extends { id: string }>(xs: T[]): Record<string, T> => Object.fromEntries(xs.map((x) => [x.id, x]))
const runs = { r1: create(RunSchema, { id: 'r1', status: RunStatus.OPEN, createdAt: timestampFromMs(1) }) }

describe('buildTrees', () => {
  it('nests workers under the lead and read-only agents under their worker', () => {
    const [t] = buildTrees(runs, byId([
      a('rev', { parentId: 'w1', role: AgentRole.REVIEWER, createdAt: timestampFromMs(4000) }),
      a('w1', { parentId: 'lead', role: AgentRole.WORKER, createdAt: timestampFromMs(3000) }),
      a('w0', { parentId: 'lead', role: AgentRole.WORKER, createdAt: timestampFromMs(2000) }),
      a('lead', { role: AgentRole.ROOT }),
    ]))
    expect(t!.roots.map((n) => n.agent.id)).toEqual(['lead'])
    expect(t!.roots[0]!.children.map((n) => n.agent.id)).toEqual(['w0', 'w1'])
    expect(t!.roots[0]!.children[1]!.children.map((n) => n.agent.id)).toEqual(['rev'])
  })

  it('shows an agent whose parent is unknown as a root, so nothing disappears', () => {
    const [t] = buildTrees(runs, byId([a('x', { parentId: 'gone' })]))
    expect(t!.roots.map((n) => n.agent.id)).toEqual(['x'])
  })

  it('still shows agents whose parents form a loop', () => {
    const [t] = buildTrees(runs, byId([a('p', { parentId: 'q', createdAt: timestampFromMs(1000) }), a('q', { parentId: 'p', createdAt: timestampFromMs(2000) }), a('ok')]))
    const ids: string[] = []
    const walk = (ns: NonNullable<typeof t>["roots"]) => ns.forEach((n) => (ids.push(n.agent.id), walk(n.children)))
    walk(t!.roots)
    expect(ids.sort()).toEqual(['ok', 'p', 'q'])
  })

  it('counts what needs the user and what is alive, and sorts runs needing the user first', () => {
    const two = { ...runs, r2: create(RunSchema, { id: 'r2', status: RunStatus.OPEN, createdAt: timestampFromMs(99_000) }) }
    const trees = buildTrees(two, byId([
      a('p', { state: AgentState.NEEDS_INPUT }),
      a('q', { state: AgentState.EXITED }),
      a('z', { runId: 'r2', state: AgentState.WORKING }),
    ]))
    expect(trees.map((t) => t.run.id)).toEqual(['r1', 'r2'])
    expect(trees[0]).toMatchObject({ needsInput: 1, live: 1 })
  })
})

describe('statusSentence', () => {
  it('says what each state means in plain words', () => {
    expect(statusSentence(a('x', { state: AgentState.WORKING }))).toBe('Working')
    expect(statusSentence(a('x', { state: AgentState.NEEDS_INPUT, stateDetail: 'Bash', needsInput: 2 }))).toBe('Needs you (2): Bash')
    expect(statusSentence(a('x', { state: AgentState.EXITED, exitCode: 0 }))).toBe('Finished')
    expect(statusSentence(a('x', { state: AgentState.EXITED, exitCode: 7 }))).toBe('Exited with code 7')
    expect(statusSentence(a('x', { state: AgentState.EXITED, stateDetail: 'lost' }))).toBe('Lost: its terminal is gone')
    // What the host stores: a stop (signal) says `killed by signal`; a lost terminal is exit code -1 with no detail.
    expect(statusSentence(a('x', { state: AgentState.EXITED, exitCode: -1, stateDetail: 'killed by signal' }))).toBe('Stopped')
    expect(statusSentence(a('x', { state: AgentState.EXITED, exitCode: -1, stateDetail: '' }))).toBe('Lost: its terminal is gone')
    // A stop recorded for a process already gone (the host stores `stopped`), not a lost terminal.
    expect(statusSentence(a('x', { state: AgentState.EXITED, exitCode: -1, stateDetail: 'stopped' }))).toBe('Stopped')
    // A failed reopen says why, stored by the host and in the live event.
    expect(statusSentence(a('x', { state: AgentState.EXITED, exitCode: -1, stateDetail: 'reopen failed: no such model' }))).toBe('Exited: reopen failed: no such model')
    // The live event's reason, until the agent is reloaded.
    expect(statusSentence(a('x', { state: AgentState.EXITED, exitCode: -1, stateDetail: 'killed' }))).toBe('Stopped')
    expect(statusSentence(a('x', { state: AgentState.UNKNOWN }))).toMatch(/unknown/)
  })
})

describe('repos', () => {
  it('names a repo by its folder', () => {
    expect(repoName('/home/u/projects/agent-mesh/')).toBe('agent-mesh')
  })
  it('lists known repos most recent first', () => {
    expect(knownRepos(byId([a('1', { repo: '/a', createdAt: timestampFromMs(1000) }), a('2', { repo: '/b', createdAt: timestampFromMs(2000) }), a('3', { repo: '/a', createdAt: timestampFromMs(500) })]))).toEqual(['/b', '/a'])
  })
})

describe('sameRepo', () => {
  it('reads a listed folder as the host does: a trailing slash and a leading ~ are the same folder', () => {
    expect(sameRepo('/home/u/projects/shop/', '/home/u/projects/shop')).toBe(true)
    expect(sameRepo('~/projects/shop', '/home/u/projects/shop')).toBe(true)
    expect(sameRepo('~/projects/shop/', '/Users/u/projects/shop')).toBe(true)
    expect(sameRepo('~/projects/shop', '/root/projects/shop')).toBe(true)
  })
  it('matches a ~ spelling on either side, and keeps "/" a slash', () => {
    expect(sameRepo('/home/u/projects/shop', '~/projects/shop')).toBe(true)
    expect(sameRepo('~/projects/shop', '~/projects/shop/')).toBe(true)
    expect(sameRepo('///', '/')).toBe(true)
    expect(sameRepo('///', '/home/u')).toBe(false)
  })
  it('keeps different folders apart', () => {
    expect(sameRepo('~/projects/shop', '/home/u/projects/shop-api')).toBe(false)
    expect(sameRepo('~/projects/shop', '/srv/x/projects/shop')).toBe(false)
    expect(sameRepo('~/projects/shop', '/home/u/v/projects/shop')).toBe(false)
    expect(sameRepo('projects/shop', '/home/u/projects/shop')).toBe(false) // a relative entry matches nothing
  })
})

describe('joinWaiting', () => {
  const msg = (id: string, type: MessageType, from: string, to: string, ms: number) =>
    create(MessageSchema, { id, runId: 'r1', type, from: { $typeName: 'agentmesh.v1.Actor', id: from, name: from, class: 0, runId: 'r1' }, toId: to, createdAt: timestampFromMs(ms), body: id })
  const team = byId([a('lead', { role: AgentRole.ROOT }), a('w1', { role: AgentRole.WORKER, parentId: 'lead' }), a('w2', { role: AgentRole.WORKER, parentId: 'lead' })])
  const row = (kind: WaitingKind, agentId: string, message?: ReturnType<typeof msg>) => create(WaitingSchema, { kind, runId: 'r1', agentId, message })

  it('shows the host\'s rows with their agent, run and message, in the host\'s order', () => {
    const p = msg('p', MessageType.PLAN, 'w1', 'lead', 1000)
    const out = joinWaiting([row(WaitingKind.PLAN, 'w1', p), row(WaitingKind.QUESTION, 'w2', msg('q', MessageType.QUESTION, 'w2', 'lead', 2000)), row(WaitingKind.INPUT, 'lead')], runs, team)
    expect(out.map((w) => `${w.kind}:${w.agent.id}:${w.message?.id ?? '-'}`)).toEqual(['plan:w1:p', 'question:w2:q', 'input:lead:-'])
    expect(out[0]!.run?.id).toBe('r1')
  })

  it('leaves out a row whose agent has not reached the store, and a kind this app does not know', () => {
    expect(joinWaiting([row(WaitingKind.PLAN, 'ghost'), row(WaitingKind.UNSPECIFIED, 'w1')], runs, team)).toEqual([])
  })

  it('messagesOf gives an agent\'s conversation in order', () => {
    const all = byId([msg('b', MessageType.ANSWER, 'lead', 'w1', 2000), msg('a', MessageType.PLAN, 'w1', 'lead', 1000), msg('c', MessageType.INFO, 'w2', 'lead', 1500)])
    expect(messagesOf(all, 'w1').map((m) => m.id)).toEqual(['a', 'b'])
  })
})
