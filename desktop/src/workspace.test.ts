import { describe, expect, it } from 'vitest'
import { MAX_SIDE_PANES, activate, closeTab, cycleTab, emptyWorkspace, focusPane, forgetHost, noteSpawned, openInBackground, pinSplit, projectTabs, pruneDismissed, setPaneTab, unsplit, type WorkspaceState } from './workspace'
import type { AgentTab } from './model'
import { AgentState, type Agent } from './gen/agentmesh/v1/types_pb'

type S = WorkspaceState & { hostId?: string; agentId?: string; tab: AgentTab }
const base = (o: Partial<S> = {}): S => ({ ...emptyWorkspace(), tab: 'chat', ...o })
const apply = (s: S, patch: Partial<S>): S => ({ ...s, ...patch })

describe('activate', () => {
  it('opens an agent as a tab once, and makes it the open one', () => {
    let s = apply(base(), activate(base(), 'h', 'a'))
    s = apply(s, activate(s, 'h', 'b'))
    s = apply(s, activate(s, 'h', 'a'))
    expect(s.tabs).toEqual([{ hostId: 'h', agentId: 'a' }, { hostId: 'h', agentId: 'b' }])
    expect(s).toMatchObject({ hostId: 'h', agentId: 'a' })
  })

  it('keeps the view each tab was on, unless one is asked for', () => {
    let s = apply(base(), activate(base(), 'h', 'a', 'terminal'))
    s = apply(s, activate(s, 'h', 'b'))
    expect(s.tab).toBe('chat')
    s = apply(s, activate(s, 'h', 'a'))
    expect(s.tab).toBe('terminal')
    s = apply(s, activate(s, 'h', 'b', 'terminal'))
    s = apply(s, activate(s, 'h', 'a'))
    expect(s.views['h/b']).toBe('terminal')
  })

  it('clears the new mark of the agent that is looked at', () => {
    const s = base({ newTiles: { 'h/a': 5, 'h/b': 6 } })
    expect(activate(s, 'h', 'a').newTiles).toEqual({ 'h/b': 6 })
  })
})

describe('closeTab', () => {
  const three = (active: string): S => base({ tabs: ['a', 'b', 'c'].map((agentId) => ({ hostId: 'h', agentId })), hostId: 'h', agentId: active, views: { 'h/c': 'terminal' } })

  it('shows the tab to the right of the one closed, else the left, else none', () => {
    expect(apply(three('b'), closeTab(three('b'), { hostId: 'h', agentId: 'b' }))).toMatchObject({ agentId: 'c', tab: 'terminal' })
    expect(apply(three('c'), closeTab(three('c'), { hostId: 'h', agentId: 'c' }))).toMatchObject({ agentId: 'b' })
    const one = base({ tabs: [{ hostId: 'h', agentId: 'a' }], hostId: 'h', agentId: 'a' })
    const closed = apply(one, closeTab(one, { hostId: 'h', agentId: 'a' }))
    expect(closed.agentId).toBeUndefined()
    expect(closed.tabs).toEqual([])
  })

  it("shows the next tab of the closed one's project, as its tab bar does", () => {
    const s = base({ tabs: ['a1', 'b1', 'a2', 'b2'].map((agentId) => ({ hostId: 'h', agentId })), hostId: 'h', agentId: 'a1' })
    const projectOf = (t: { agentId: string }) => t.agentId[0]
    expect(apply(s, closeTab(s, { hostId: 'h', agentId: 'a1' }, projectOf))).toMatchObject({ agentId: 'a2' })
    const last = { ...s, agentId: 'b2' }
    expect(apply(last, closeTab(last, { hostId: 'h', agentId: 'b2' }, projectOf))).toMatchObject({ agentId: 'b1' })
    // The project's last tab: the next tab, whatever its project.
    const lone = base({ tabs: ['a1', 'b1'].map((agentId) => ({ hostId: 'h', agentId })), hostId: 'h', agentId: 'a1' })
    expect(apply(lone, closeTab(lone, { hostId: 'h', agentId: 'a1' }, projectOf))).toMatchObject({ agentId: 'b1' })
  })

  it('leaves what shows alone when another tab is closed', () => {
    const s = three('a')
    expect(closeTab(s, { hostId: 'h', agentId: 'c' })).not.toHaveProperty('agentId')
    expect(closeTab(s, { hostId: 'h', agentId: 'nope' })).toEqual({})
  })
})

describe('cycleTab', () => {
  const s = base({ tabs: ['a', 'b', 'c'].map((agentId) => ({ hostId: 'h', agentId })), hostId: 'h', agentId: 'c' })
  it('wraps both ways', () => {
    expect(cycleTab(s, 1)?.agentId).toBe('a')
    expect(cycleTab({ ...s, agentId: 'a' }, -1)?.agentId).toBe('c')
  })
  it('starts at an end when no tab is open, and does nothing without tabs', () => {
    expect(cycleTab({ ...s, agentId: undefined }, 1)?.agentId).toBe('a')
    expect(cycleTab({ ...s, agentId: undefined }, -1)?.agentId).toBe('c')
    expect(cycleTab(base(), 1)).toBeUndefined()
  })
})

describe('pruneDismissed', () => {
  const agents = { a: { state: AgentState.IDLE }, b: { state: AgentState.WORKING } } as unknown as Record<string, Agent>
  it('forgets a dismissal once the agent left the state it was dismissed in, only on its host, and keeps one of an agent not listed (a reload)', () => {
    const held = { 'h/a': AgentState.IDLE, 'h/b': AgentState.IDLE, 'h/gone': AgentState.IDLE, 'g/a': AgentState.NEEDS_INPUT }
    expect(pruneDismissed(held, 'h', agents)).toEqual({ 'h/a': AgentState.IDLE, 'h/gone': AgentState.IDLE, 'g/a': AgentState.NEEDS_INPUT })
  })
  it('is the same object when nothing changed', () => {
    const held = { 'h/a': AgentState.IDLE }
    expect(pruneDismissed(held, 'h', agents)).toBe(held)
  })
})

describe('forgetHost and noteSpawned', () => {
  it('drops a removed host\'s tabs and marks', () => {
    const s = base({ tabs: [{ hostId: 'h', agentId: 'a' }, { hostId: 'g', agentId: 'a' }], views: { 'h/a': 'terminal', 'g/a': 'terminal' }, newTiles: { 'h/a': 1 }, dismissed: { 'h/a': 1 } })
    expect(forgetHost(s, 'h')).toEqual({ tabs: [{ hostId: 'g', agentId: 'a' }], side: [], views: { 'g/a': 'terminal' }, newTiles: {}, dismissed: {} })
  })

  it('does what the setting says, and never focuses', () => {
    const ref = { hostId: 'h', agentId: 'n' }
    expect(noteSpawned(base(), ref, 'dock', 7)).toEqual({ newTiles: { 'h/n': 7 }, dockBadge: 0 })
    expect(noteSpawned(base({ dockOpen: false }), ref, 'dock', 7)).toMatchObject({ dockBadge: 1 })
    expect(noteSpawned(base(), ref, 'badge', 7)).toEqual({ dockBadge: 1 })
    expect(noteSpawned(base(), ref, 'tab', 7)).toEqual({ tabs: [ref] })
    expect(noteSpawned(base(), ref, 'none', 7)).toEqual({})
    // A tab that is already open stays one.
    expect(openInBackground(base({ tabs: [ref] }), ref)).toEqual({})
  })
})

describe('splits', () => {
  const ref = (agentId: string) => ({ hostId: 'h', agentId })
  const open = (...ids: string[]): S => base({ tabs: ids.map(ref), hostId: 'h', agentId: ids[0], tab: 'chat', views: { 'h/b': 'terminal' } })

  it('pins an agent beside the main one, opening its tab, on the view it was last on', () => {
    let s = apply(open('a'), pinSplit(open('a'), ref('b')))
    expect(s.side).toEqual([{ ...ref('b'), tab: 'terminal' }])
    expect(s.tabs).toEqual([ref('a'), ref('b')])
    s = apply(s, pinSplit(s, ref('c'), 'terminal'))
    expect(s.side.map((p) => [p.agentId, p.tab])).toEqual([['b', 'terminal'], ['c', 'terminal']])
  })

  it('does not pin the main agent, one already beside, or more than the limit', () => {
    let s = open('a')
    expect(pinSplit(s, ref('a'))).toEqual({})
    s = apply(s, pinSplit(s, ref('b')))
    expect(pinSplit(s, ref('b'))).toEqual({})
    for (const id of ['c', 'd', 'e', 'f']) s = apply(s, pinSplit(s, ref(id)))
    expect(s.side).toHaveLength(MAX_SIDE_PANES)
  })

  it('takes a pane away without closing its tab, and changes the view of one', () => {
    let s = apply(open('a'), pinSplit(open('a'), ref('b')))
    s = apply(s, setPaneTab(s, ref('b'), 'terminal'))
    expect(s.side[0]!.tab).toBe('terminal')
    expect(setPaneTab(s, ref('b'), 'terminal')).toEqual({})
    s = apply(s, unsplit(s, ref('b')))
    expect(s.side).toEqual([])
    expect(s.tabs).toHaveLength(2)
  })

  it('swaps a pane with the main one, and the main one takes its place', () => {
    let s = apply(open('a'), pinSplit(open('a'), ref('b')))
    s = apply(s, pinSplit(s, ref('c')))
    s = apply(s, focusPane(s, ref('b')))
    expect(s).toMatchObject({ agentId: 'b', tab: 'terminal' })
    expect(s.side.map((p) => [p.agentId, p.tab])).toEqual([['a', 'chat'], ['c', 'chat']])
    expect(s.views['h/b']).toBe('terminal')
  })

  it('shows a pane as the main one when its agent is activated, never twice', () => {
    let s = apply(open('a'), pinSplit(open('a'), ref('b')))
    s = apply(s, activate(s, 'h', 'b'))
    expect(s).toMatchObject({ agentId: 'b', side: [] })
  })

  it('closing the main agent brings the first pane over; closing a pane\'s tab closes the pane', () => {
    let s = apply(open('a'), pinSplit(open('a'), ref('b')))
    s = apply(s, pinSplit(s, ref('c')))
    const closedMain = apply(s, closeTab(s, ref('a')))
    expect(closedMain).toMatchObject({ agentId: 'b', tab: 'terminal' })
    expect(closedMain.side.map((p) => p.agentId)).toEqual(['c'])
    const closedPane = apply(s, closeTab(s, ref('b')))
    expect(closedPane.agentId).toBe('a')
    expect(closedPane.side.map((p) => p.agentId)).toEqual(['c'])
  })

  it('forgets the panes of a removed host', () => {
    const s = base({ tabs: [{ hostId: 'x', agentId: 'a' }], side: [{ hostId: 'x', agentId: 'a', tab: 'chat' }, { hostId: 'y', agentId: 'b', tab: 'chat' }] })
    expect(forgetHost(s, 'x').side).toEqual([{ hostId: 'y', agentId: 'b', tab: 'chat' }])
  })
})

describe('projectTabs', () => {
  const t = (agentId: string) => ({ hostId: 'h', agentId })
  const of: Record<string, string> = { a: 'p1', b: 'p2', c: 'p1' }
  const projectOf = (r: { agentId: string }) => of[r.agentId]
  it('shows the open agent\'s project\'s tabs, all of them with none open or one not known yet', () => {
    const tabs = [t('a'), t('b'), t('c'), t('x')]
    // x's project isn't known yet (its host is connecting): it stays in reach.
    expect(projectTabs(tabs, projectOf, t('a')).map((r) => r.agentId)).toEqual(['a', 'c', 'x'])
    expect(projectTabs(tabs, projectOf, t('b')).map((r) => r.agentId)).toEqual(['b', 'x'])
    expect(projectTabs(tabs, projectOf, undefined)).toHaveLength(4)
    expect(projectTabs(tabs, projectOf, t('x'))).toHaveLength(4)
  })
})
