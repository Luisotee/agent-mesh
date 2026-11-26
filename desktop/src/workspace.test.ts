import { describe, expect, it } from 'vitest'
import { activate, closeTab, cycleTab, emptyWorkspace, forgetHost, noteSpawned, openInBackground, pruneDismissed, type WorkspaceState } from './workspace'
import type { AgentTab } from './model'
import { AgentState, type Agent } from './gen/agentmesh/v1/types_pb'

type S = WorkspaceState & { hostId?: string; agentId?: string; tab: AgentTab }
const base = (o: Partial<S> = {}): S => ({ ...emptyWorkspace(), tab: 'overview', ...o })
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
    expect(s.tab).toBe('overview')
    s = apply(s, activate(s, 'h', 'a'))
    expect(s.tab).toBe('terminal')
    s = apply(s, activate(s, 'h', 'b', 'messages'))
    s = apply(s, activate(s, 'h', 'a'))
    expect(s.views['h/b']).toBe('messages')
  })

  it('clears the new mark of the agent that is looked at', () => {
    const s = base({ newTiles: { 'h/a': 5, 'h/b': 6 } })
    expect(activate(s, 'h', 'a').newTiles).toEqual({ 'h/b': 6 })
  })
})

describe('closeTab', () => {
  const three = (active: string): S => base({ tabs: ['a', 'b', 'c'].map((agentId) => ({ hostId: 'h', agentId })), hostId: 'h', agentId: active, views: { 'h/c': 'messages' } })

  it('shows the tab to the right of the one closed, else the left, else none', () => {
    expect(apply(three('b'), closeTab(three('b'), { hostId: 'h', agentId: 'b' }))).toMatchObject({ agentId: 'c', tab: 'messages' })
    expect(apply(three('c'), closeTab(three('c'), { hostId: 'h', agentId: 'c' }))).toMatchObject({ agentId: 'b' })
    const one = base({ tabs: [{ hostId: 'h', agentId: 'a' }], hostId: 'h', agentId: 'a' })
    const closed = apply(one, closeTab(one, { hostId: 'h', agentId: 'a' }))
    expect(closed.agentId).toBeUndefined()
    expect(closed.tabs).toEqual([])
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
  it('forgets a dismissal once the agent left the state it was dismissed in, or is gone, and only on its host', () => {
    const held = { 'h/a': AgentState.IDLE, 'h/b': AgentState.IDLE, 'h/gone': AgentState.IDLE, 'g/a': AgentState.NEEDS_INPUT }
    expect(pruneDismissed(held, 'h', agents)).toEqual({ 'h/a': AgentState.IDLE, 'g/a': AgentState.NEEDS_INPUT })
  })
  it('is the same object when nothing changed', () => {
    const held = { 'h/a': AgentState.IDLE }
    expect(pruneDismissed(held, 'h', agents)).toBe(held)
  })
})

describe('forgetHost and noteSpawned', () => {
  it('drops a removed host\'s tabs and marks', () => {
    const s = base({ tabs: [{ hostId: 'h', agentId: 'a' }, { hostId: 'g', agentId: 'a' }], views: { 'h/a': 'terminal', 'g/a': 'messages' }, newTiles: { 'h/a': 1 }, dismissed: { 'h/a': 1 } })
    expect(forgetHost(s, 'h')).toEqual({ tabs: [{ hostId: 'g', agentId: 'a' }], views: { 'g/a': 'messages' }, newTiles: {}, dismissed: {} })
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
