import { createStore, type StoreApi } from 'zustand/vanilla'
import type { Agent, Cursor, Event, Run } from './gen/agentmesh/v1/types_pb'
import { AgentState } from './gen/agentmesh/v1/types_pb'
import type { HelloResponse } from './gen/agentmesh/v1/mesh_pb'
import type { ConnState, Frame } from './connection'

export interface LogLine {
  at: number
  text: string
}

/** One host as the app knows it: what its snapshot and event stream said, and the state of the connection. */
export interface HostState {
  conn: ConnState
  /** What the host last said about itself, kept while it is unreachable so the view doesn't change under the user. */
  daemon?: HelloResponse
  /** True once a snapshot or a resume arrived on the current connection. */
  synced: boolean
  runs: Record<string, Run>
  agents: Record<string, Agent>
  cursor?: Cursor
  log: LogLine[]
}

export type HostStore = StoreApi<HostState>

export function emptyHost(): HostState {
  return { conn: { kind: 'connecting' }, synced: false, runs: {}, agents: {}, log: [] }
}

const MAX_LOG = 200

/**
 * Applies one frame of the event stream. A snapshot replaces runs and agents (a resync); a resume keeps them;
 * an event changes one thing. Pure, so the stream and its replay can be checked to give the same state.
 */
export function applyFrame(s: HostState, frame: Frame): HostState {
  switch (frame.case) {
    case 'snapshot': {
      const runs: Record<string, Run> = {}
      for (const r of frame.value.runs) runs[r.id] = r
      const agents: Record<string, Agent> = {}
      for (const a of frame.value.agents) agents[a.id] = a
      return { ...s, synced: true, runs, agents, cursor: frame.value.cursor }
    }
    case 'resumed':
      return { ...s, synced: true }
    case 'event':
      return applyEvent(s, frame.value)
    default:
      return s
  }
}

function applyEvent(s: HostState, e: Event): HostState {
  const cursor: Cursor = { $typeName: 'agentmesh.v1.Cursor', generation: e.generation, seq: e.seq }
  const next = { ...s, cursor }
  const p = e.payload
  switch (p.case) {
    case 'runStarted':
      if (p.value.run) return { ...next, runs: { ...s.runs, [p.value.run.id]: p.value.run } }
      return next
    case 'agentSpawned':
      if (p.value.agent) return { ...next, agents: { ...s.agents, [p.value.agent.id]: p.value.agent } }
      return next
    case 'agentStateChanged': {
      const a = s.agents[e.agentId]
      if (!a) return next
      const changed: Agent = {
        ...a,
        state: p.value.to,
        stateDetail: p.value.detail,
        needsInput: p.value.needsInput,
        lastActivity: e.time ?? a.lastActivity,
      }
      return { ...next, agents: { ...s.agents, [a.id]: changed } }
    }
    case 'agentExited': {
      const a = s.agents[e.agentId]
      if (!a) return next
      const changed: Agent = {
        ...a,
        state: AgentState.EXITED,
        exitCode: p.value.exitCode,
        stateDetail: p.value.reason,
        needsInput: 0,
        lastActivity: e.time ?? a.lastActivity,
      }
      return { ...next, agents: { ...s.agents, [a.id]: changed } }
    }
    case 'agentAdopted': {
      const a = s.agents[e.agentId]
      return a ? { ...next, agents: { ...s.agents, [a.id]: { ...a, terminalId: p.value.terminalId } } } : next
    }
    default:
      return next
  }
}

export function createHostStore(): HostStore {
  return createStore<HostState>(() => emptyHost())
}

/** What the connection feeds into a store. */
export function storeSink(store: HostStore) {
  return {
    onState: (conn: ConnState) =>
      store.setState(() => (conn.kind === 'ready' ? { conn, daemon: conn.hello } : { conn, synced: false })),
    onFrame: (f: Frame) => store.setState((s) => applyFrame(s, f)),
    cursor: () => store.getState().cursor,
    log: (text: string) =>
      store.setState((s) => ({ log: [...s.log.slice(-(MAX_LOG - 1)), { at: Date.now(), text }] })),
  }
}
