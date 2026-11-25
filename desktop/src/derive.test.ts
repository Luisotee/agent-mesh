import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { buildTrees, knownRepos, messagesOf, repoName, statusSentence, waitingOnYou } from './derive'
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
    // Ended by a signal (exit code -1), whether or not the reason survived a reload from the host.
    expect(statusSentence(a('x', { state: AgentState.EXITED, exitCode: -1 }))).toBe('Stopped')
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

describe('waitingOnYou', () => {
  const msg = (id: string, type: MessageType, from: string, to: string, ms: number) =>
    create(MessageSchema, { id, runId: 'r1', type, from: { $typeName: 'agentmesh.v1.Actor', id: from, name: from, class: 0, runId: 'r1' }, toId: to, createdAt: timestampFromMs(ms), body: id })
  const team = byId([a('lead', { role: AgentRole.ROOT }), a('w1', { role: AgentRole.WORKER, parentId: 'lead' }), a('w2', { role: AgentRole.WORKER, parentId: 'lead' })])
  const ids = (m: ReturnType<typeof msg>[], agents = team) => waitingOnYou(runs, agents, byId(m)).map((w) => `${w.kind}:${w.agent.id}`)

  it('lists a plan or question to the lead that nobody answered, oldest first', () => {
    expect(ids([msg('q', MessageType.QUESTION, 'w2', 'lead', 3000), msg('p', MessageType.PLAN, 'w1', 'lead', 2000)])).toEqual(['plan:w1', 'question:w2'])
  })

  it('closes an item once an answer to its author follows, from the lead or the user', () => {
    expect(ids([msg('p', MessageType.PLAN, 'w1', 'lead', 1000), msg('a', MessageType.ANSWER, 'lead', 'w1', 2000)])).toEqual([])
    // An answer before the plan, or to someone else, doesn't close it.
    expect(ids([msg('a', MessageType.ANSWER, 'lead', 'w1', 500), msg('p', MessageType.PLAN, 'w1', 'lead', 1000)])).toEqual(['plan:w1'])
    expect(ids([msg('p', MessageType.PLAN, 'w1', 'lead', 1000), msg('a', MessageType.ANSWER, 'lead', 'w2', 2000)])).toEqual(['plan:w1'])
  })

  it('a second plan after an answer waits again', () => {
    expect(ids([msg('p1', MessageType.PLAN, 'w1', 'lead', 1000), msg('a', MessageType.ANSWER, 'lead', 'w1', 2000), msg('p2', MessageType.PLAN, 'w1', 'lead', 3000)])).toEqual(['plan:w1'])
  })

  it('ignores what is not for the lead, reports, and authors that exited', () => {
    expect(ids([msg('p', MessageType.PLAN, 'w1', 'w2', 1000), msg('d', MessageType.DONE, 'w1', 'lead', 2000)])).toEqual([])
    const gone = { ...team, w1: a('w1', { role: AgentRole.WORKER, parentId: 'lead', state: AgentState.EXITED }) }
    expect(ids([msg('p', MessageType.PLAN, 'w1', 'lead', 1000)], gone)).toEqual([])
  })

  it('adds the agents stopped at a prompt, after the plans and questions', () => {
    const t = { ...team, w2: a('w2', { role: AgentRole.WORKER, parentId: 'lead', state: AgentState.NEEDS_INPUT }) }
    expect(ids([msg('p', MessageType.PLAN, 'w1', 'lead', 1000)], t)).toEqual(['plan:w1', 'input:w2'])
  })

  it('has nothing for a closed run, or one with no lead', () => {
    const closed = { r1: create(RunSchema, { id: 'r1', status: RunStatus.CLOSED }) }
    expect(waitingOnYou(closed, team, byId([msg('p', MessageType.PLAN, 'w1', 'lead', 1)]))).toEqual([])
    const leadless = byId([a('w1', { role: AgentRole.WORKER })])
    expect(waitingOnYou(runs, leadless, byId([msg('p', MessageType.PLAN, 'w1', 'lead', 1)]))).toEqual([])
  })

  it('messagesOf gives an agent\'s conversation in order', () => {
    const all = byId([msg('b', MessageType.ANSWER, 'lead', 'w1', 2000), msg('a', MessageType.PLAN, 'w1', 'lead', 1000), msg('c', MessageType.INFO, 'w2', 'lead', 1500)])
    expect(messagesOf(all, 'w1').map((m) => m.id)).toEqual(['a', 'b'])
  })
})
