import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { askLines, runAutonomyOf, runLevelOf } from './autonomy'
import { waitingCount } from './derive'
import { githubNote } from './buckets'
import { layoutOf, parseLayout, pruneHost, restoreOf, type LayoutSource } from './layout'
import { ModelSwapSchema, ProjectSchema, WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentSchema, AgentState, EventSchema, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { appPrefs } from './prefs'
import { rankProjects } from './projects'
import { historyRows } from './side-panel'
import { applyFrame, emptyHost } from './store'
import { emptyWorkspace, splitAtEdge, type WorkspaceState } from './workspace'

describe('runAutonomyOf', () => {
  it('sends nothing when the host\'s settings apply as they are', () => {
    expect(runAutonomyOf('inherit', '')).toBeUndefined()
    expect(runAutonomyOf('inherit', '  \n ')).toBeUndefined()
  })
  it('keeps the host\'s repo lists when only lines are added', () => {
    expect(runAutonomyOf('inherit', 'deploys\nmigrations')).toEqual({ userApprovesPlans: false, userMerges: false, askBefore: ['deploys', 'migrations'], keepRepoLists: true })
  })
  it('replaces them with a level for this run: approving plans means merging too', () => {
    expect(runAutonomyOf('plans', '')).toEqual({ userApprovesPlans: true, userMerges: true, askBefore: [], keepRepoLists: false })
    expect(runAutonomyOf('merges', 'x')).toEqual({ userApprovesPlans: false, userMerges: true, askBefore: ['x'], keepRepoLists: false })
    expect(runAutonomyOf('free', '')).toEqual({ userApprovesPlans: false, userMerges: false, askBefore: [], keepRepoLists: false })
  })
  it('reads back the level a run started with, and nothing when the repo lists still apply', () => {
    expect(runLevelOf(undefined)).toBeUndefined()
    expect(runLevelOf({ userApprovesPlans: false, userMerges: false, keepRepoLists: true })).toBeUndefined()
    expect(runLevelOf({ userApprovesPlans: true, userMerges: true, keepRepoLists: false })).toBe('plans')
    expect(runLevelOf({ userApprovesPlans: false, userMerges: true, keepRepoLists: false })).toBe('merges')
    expect(runLevelOf({ userApprovesPlans: false, userMerges: false, keepRepoLists: false })).toBe('free')
  })
  it('reads lines trimmed, without blanks or repeats', () => {
    expect(askLines(' a \n\nb\na\n')).toEqual(['a', 'b'])
  })
})

describe('waitingCount', () => {
  const agents = { a: create(AgentSchema, { id: 'a' }), b: create(AgentSchema, { id: 'b' }) }
  const input = (agentId: string) => create(WaitingSchema, { kind: WaitingKind.INPUT, agentId })
  it('counts a held prompt once instead of the terminal row of its agent, and one an agent has no row for', () => {
    expect(waitingCount([input('a')], [{ agentId: 'a' }], {}, agents)).toBe(1)
    expect(waitingCount([input('a')], [{ agentId: 'a' }, { agentId: 'a' }], {}, agents)).toBe(2)
    expect(waitingCount([input('a')], [{ agentId: 'b' }], {}, agents)).toBe(2)
    expect(waitingCount([], [{ agentId: 'gone' }], {}, agents)).toBe(0)
  })
})

describe('rankProjects', () => {
  const p = (path: string, o: { inReposDir?: boolean; last?: number } = {}) =>
    create(ProjectSchema, { path, name: path.split('/').pop()!, inReposDir: o.inReposDir ?? true, lastUsed: o.last ? { seconds: BigInt(o.last), nanos: 0 } : undefined } as never)
  const all = [p('/p/web'), p('/p/api', { last: 10 }), p('/p/shop', { last: 20 }), p('/x/other', { inReposDir: false })]

  it('puts the pinned first in pin order, then the host\'s folders by last use, then the rest', () => {
    expect(rankProjects(all, '', ['/p/web', '/p/api']).map((x) => x.name)).toEqual(['web', 'api', 'shop', 'other'])
    expect(rankProjects(all, '', []).map((x) => x.name)).toEqual(['shop', 'api', 'web', 'other'])
  })
  it('matches every word of the query in the name or the path, in any case', () => {
    expect(rankProjects(all, 'P WE', []).map((x) => x.name)).toEqual(['web'])
    expect(rankProjects(all, '/x/', []).map((x) => x.name)).toEqual(['other'])
    expect(rankProjects(all, 'nothing', [])).toEqual([])
  })
})

describe('splitAtEdge', () => {
  const ws = (o: Partial<WorkspaceState> = {}): WorkspaceState & { hostId?: string; agentId?: string; tab: 'overview' } => ({ ...emptyWorkspace(), tab: 'overview', hostId: 'h', agentId: 'a', tabs: [{ hostId: 'h', agentId: 'a' }, { hostId: 'h', agentId: 'b' }], ...o })
  const b = { hostId: 'h', agentId: 'b' }

  it('puts the dropped agent beside the open one on the right or bottom edge, side by side or stacked', () => {
    expect(splitAtEdge(ws(), b, 'right')).toMatchObject({ splitDir: 'row', side: [{ ...b, tab: 'overview' }] })
    expect(splitAtEdge(ws(), b, 'bottom')).toMatchObject({ splitDir: 'column', side: [{ ...b, tab: 'overview' }] })
  })
  it('makes it the main pane on the left or top edge, and the open agent goes beside it', () => {
    const left = splitAtEdge(ws(), b, 'left')
    expect(left).toMatchObject({ splitDir: 'row', agentId: 'b', side: [{ hostId: 'h', agentId: 'a' }] })
    expect(splitAtEdge(ws(), b, 'top')).toMatchObject({ splitDir: 'column', agentId: 'b' })
  })
  it('opens a tab that is not open yet, and does nothing for the open agent, one already beside, or a full split', () => {
    expect(splitAtEdge(ws({ tabs: [{ hostId: 'h', agentId: 'a' }] }), b, 'right').tabs).toHaveLength(2)
    expect(splitAtEdge(ws(), { hostId: 'h', agentId: 'a' }, 'right')).toEqual({})
    expect(splitAtEdge(ws({ side: [{ ...b, tab: 'chat' }] }), b, 'right')).toEqual({})
    const full = ['c', 'd', 'e'].map((agentId) => ({ hostId: 'h', agentId, tab: 'chat' as const }))
    expect(splitAtEdge(ws({ side: full }), b, 'right')).toEqual({})
  })
})

describe('the layout keeps dismissed tiles and pinned projects', () => {
  const src = (o: Partial<LayoutSource> = {}): LayoutSource => ({ ...emptyWorkspace(), tab: 'overview', sidebarGroup: 'run', panelOpen: true, ...o })

  it('round-trips them for the hosts that are still there', () => {
    const l = parseLayout(JSON.stringify(layoutOf(src({ dismissed: { 'h/a': AgentState.IDLE, 'g/z': AgentState.EXITED }, pins: { h: ['/p/app', '/p/app'], g: ['/q'] } }))))!
    expect(l.pins).toEqual({ h: ['/p/app'], g: ['/q'] })
    expect(restoreOf(l, new Set(['h']))).toMatchObject({ dismissed: { 'h/a': AgentState.IDLE }, pins: { h: ['/p/app'] } })
  })
  it('reads a layout from before them, and drops damaged entries', () => {
    expect(parseLayout('{"v":1}')).toMatchObject({ dismissed: {}, pins: {} })
    expect(parseLayout(JSON.stringify({ v: 1, dismissed: { nokey: 3, 'h/a': 'x', 'h/b': 4 }, pins: { h: [1, '', '/p'], g: 'no' } }))).toMatchObject({ dismissed: { 'h/b': 4 }, pins: { h: ['/p'] } })
  })
  it('forgets the dismissal of an agent the host lost', () => {
    const s = src({ dismissed: { 'h/a': AgentState.IDLE, 'h/gone': AgentState.IDLE } })
    expect(pruneHost(s, 'h', { a: create(AgentSchema, { id: 'a' }) })).toEqual({ tabs: [], side: [], views: {}, dismissed: { 'h/a': AgentState.IDLE } })
    expect(pruneHost(src({ dismissed: { 'h/a': AgentState.IDLE } }), 'h', { a: create(AgentSchema, { id: 'a' }) })).toBeUndefined()
  })
})

describe('the store', () => {
  it('renames a run from its event', () => {
    const s = { ...emptyHost(), runs: { r1: create(RunSchema, { id: 'r1', title: 'old', status: RunStatus.OPEN }) } }
    const e = create(EventSchema, { runId: 'r1', seq: 5n, generation: 'g', payload: { case: 'runRenamed', value: { $typeName: 'agentmesh.v1.RunRenamed', title: 'new', previous: 'old' } } })
    expect(applyFrame(s, { case: 'event', value: e }).runs.r1!.title).toBe('new')
    // A run the app doesn't hold is left alone.
    expect(applyFrame(emptyHost(), { case: 'event', value: e }).runs).toEqual({})
  })
})

describe('small helpers', () => {
  it('words why GitHub can\'t be read, and says nothing when it can', () => {
    expect(githubNote('no_gh')).toContain("isn't installed")
    expect(githubNote('not_logged_in')).toContain('gh auth login')
    expect(githubNote('')).toBe('')
    expect(githubNote('something new')).toBe('')
  })
  it('reads the model history as rows', () => {
    const rows = historyRows([create(ModelSwapSchema, { model: 'opus', effort: 'high', why: 'spawned', time: { seconds: 100n, nanos: 0 } as never }), create(ModelSwapSchema, { model: '', why: 'moved' })])
    expect(rows).toEqual([{ text: 'opus · high', why: 'spawned', at: 100_000 }, { text: "the CLI's default", why: 'moved', at: undefined }])
  })
  it('reads app.open_views, off until the host says', () => {
    expect(appPrefs({}).openViews).toBe(false)
  })
})
