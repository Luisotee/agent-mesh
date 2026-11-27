import { createStore, type StoreApi } from 'zustand/vanilla'
import type { Agent, Cursor, Device, Event, Message, QueuedPrompt, Run, Setting } from './gen/agentmesh/v1/types_pb'
import { AgentState, RunStatus } from './gen/agentmesh/v1/types_pb'
import type { HelloResponse, Waiting as WaitingRow } from './gen/agentmesh/v1/mesh_pb'
import type { AgentPullRequests } from './gen/agentmesh/v1/pr_pb'
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
  /** How far the host's clock is ahead of ours, in ms (negative: behind), from its last `Hello`. */
  clockSkewMs: number
  /** True once a snapshot or a resume arrived on the current connection. */
  synced: boolean
  runs: Record<string, Run>
  agents: Record<string, Agent>
  /** Every message seen: loaded with ListMessages, then added by events. */
  messages: Record<string, Message>
  /** How loading a run's stored messages went (`messages.ts`): they are read when a run's Messages tab is first shown. A run not in here wasn't asked for. */
  messageLoad: Record<string, MessageLoad>
  /** The host's settings with their schemas, by key: loaded with ListSettings, then kept by SettingChanged events. */
  settings: Record<string, Setting>
  /** What the host says waits on the user (ListWaiting), kept up to date by `refresh.ts`; empty until it first answers. */
  waiting: WaitingRow[]
  /** Each change worker's pull requests (ListPullRequests), by agent id: read at connect and every minute by `refresh.ts`, and kept by PullRequestChanged events; empty until the host first answers. */
  pullRequests: Record<string, AgentPullRequests>
  /** The devices paired with the host (ListDevices, then DevicePaired and DeviceRevoked events), once it was asked. */
  devices: Device[]
  /** The pairing each device came through (a `DevicePaired` event's `pairingId`), by device id; a device with no entry is one whose event we didn't see, or from a host before the field. */
  pairedVia: Record<string, string>
  /** The prompts sent to an agent's chat that wait to be typed, by agent id, oldest first: read with ListPrompts when its chat is shown, then kept by the Prompt* events. */
  prompts: Record<string, QueuedPrompt[]>
  /** Ids of prompts the host said were typed or taken back: a listing or an answer older than that event never brings one back. */
  promptsDone: Record<string, true>
  cursor?: Cursor
  log: LogLine[]
}

/** `loading` until the run's stored messages arrived once; `failed` when the last try did (a retry follows). */
export type MessageLoad = 'loading' | 'loaded' | 'failed'

export type HostStore = StoreApi<HostState>

export function emptyHost(): HostState {
  return { conn: { kind: 'connecting' }, synced: false, runs: {}, agents: {}, messages: {}, messageLoad: {}, settings: {}, waiting: [], pullRequests: {}, devices: [], pairedVia: {}, prompts: {}, promptsDone: {}, clockSkewMs: 0, log: [] }
}

const MAX_LOG = 200

/**
 * Applies one frame of the event stream. A snapshot replaces runs and agents (a resync); a resume keeps them;
 * an event changes one thing; an activity frame updates what a working agent is running. Pure, so the stream and its replay can be checked to give the same state.
 */
export function applyFrame(s: HostState, frame: Frame): HostState {
  switch (frame.case) {
    case 'snapshot': {
      const runs: Record<string, Run> = {}
      for (const r of frame.value.runs) runs[r.id] = r
      const agents: Record<string, Agent> = {}
      for (const a of frame.value.agents) agents[a.id] = a
      // Messages of runs the host no longer lists go; those of the others stay (and are topped up by the loader).
      const messages = Object.fromEntries(Object.entries(s.messages).filter(([, m]) => m.runId in runs))
      const messageLoad = Object.fromEntries(Object.entries(s.messageLoad).filter(([id]) => id in runs))
      return { ...s, synced: true, runs, agents, messages, messageLoad, cursor: frame.value.cursor }
    }
    case 'resumed':
      return { ...s, synced: true }
    // The tool an agent is running, live only: kept where the agent's own state detail is, with the time of it.
    case 'activity': {
      const { agentId, detail, time } = frame.value
      return withAgent(s, agentId, (a) => (a.state === AgentState.WORKING ? { ...a, stateDetail: detail, lastActivity: time ?? a.lastActivity } : a))
    }
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
    case 'messageSent': {
      const m = p.value.message
      return m ? { ...next, messages: { ...s.messages, [m.id]: m } } : next
    }
    case 'messageDelivered': {
      const m = s.messages[p.value.messageId]
      return m ? { ...next, messages: { ...s.messages, [m.id]: { ...m, deliveredAt: e.time ?? m.deliveredAt } } } : next
    }
    case 'settingChanged': {
      const cur = s.settings[p.value.key]
      if (!cur) return next
      // The event carries the effective value now; a reset means the default applies again.
      const changed: Setting = { ...cur, value: p.value.value ?? (p.value.toDefault ? cur.default : cur.value), isSet: !p.value.toDefault, updatedBy: e.actor ?? cur.updatedBy, updatedAt: e.time ?? cur.updatedAt }
      return { ...next, settings: { ...s.settings, [cur.key]: changed } }
    }
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
    // A model change reopens the same conversation (daemon/internal/daemon/reopen.go): the agent keeps its identity
    // and gets the new model, terminal and session.
    case 'agentReopenRequested':
      return withAgent(next, e.agentId, (a) => ({ ...a, pendingReopen: p.value.reopen }))
    case 'agentReopenDropped':
      return withAgent(next, e.agentId, (a) => ({ ...a, pendingReopen: undefined }))
    case 'agentReopened':
      return withAgent(next, e.agentId, (a) => ({
        ...a,
        pendingReopen: undefined,
        terminalId: p.value.terminalId,
        sessionId: p.value.sessionId,
        model: p.value.reopen?.model ?? a.model,
        effort: p.value.reopen?.effort ?? a.effort,
        denyEdits: p.value.reopen?.denyEdits ?? a.denyEdits,
      }))
    case 'agentPhaseChanged':
      return withAgent(next, e.agentId, (a) => ({ ...a, phase: p.value.to }))
    // An asker or reviewer given a change task is a worker from now on (daemon/internal/daemon/reopen.go, takeChange).
    case 'agentRoleChanged':
      return withAgent(next, e.agentId, (a) => ({ ...a, role: p.value.to }))
    // Stopped on purpose (daemon/internal/daemon/retire.go): the process is gone, a message brings it back.
    case 'agentRetired':
      return withAgent(next, e.agentId, (a) => ({ ...a, state: AgentState.RETIRED, needsInput: 0, stateDetail: '', pendingReopen: undefined, lastActivity: e.time ?? a.lastActivity }))
    case 'runStopped': {
      const r = s.runs[e.runId]
      return r ? { ...next, runs: { ...s.runs, [r.id]: { ...r, status: RunStatus.CLOSED } } } : next
    }
    // A PR's state or checks changed: it replaces what we hold for it (or is added), so the sidebar's buckets follow a merge at once.
    case 'pullRequestChanged': {
      const pr = p.value.pullRequest
      if (!pr) return next
      const cur = s.pullRequests[pr.agentId]
      const list = cur?.pullRequests ?? []
      const prs = list.some((x) => x.url === pr.url) ? list.map((x) => (x.url === pr.url ? pr : x)) : [...list, pr]
      const merged: AgentPullRequests = { ...(cur ?? { $typeName: 'agentmesh.v1.AgentPullRequests', agentId: pr.agentId, agentName: '', reviewing: [], readFailed: false }), pullRequests: prs }
      return { ...next, pullRequests: { ...s.pullRequests, [pr.agentId]: merged } }
    }
    // The chat's queue: a prompt joins it, and leaves it once typed or taken back.
    case 'promptQueued': {
      const q = p.value.prompt
      if (!q) return next
      // A requeue (its typing never reached the terminal) is queued again, so it is no longer done.
      const { [q.id]: _again, ...promptsDone } = s.promptsDone
      const list = (s.prompts[q.agentId] ?? []).filter((x) => x.id !== q.id)
      const sorted = [...list, q].sort((a, b) => Number((a.createdAt?.seconds ?? 0n) - (b.createdAt?.seconds ?? 0n)) || (a.createdAt?.nanos ?? 0) - (b.createdAt?.nanos ?? 0))
      return { ...next, promptsDone, prompts: { ...s.prompts, [q.agentId]: sorted } }
    }
    case 'promptTyped':
    case 'promptWithdrawn':
      return { ...withoutPrompt(next, e.agentId, p.value.promptId), promptsDone: { ...s.promptsDone, [p.value.promptId]: true } }
    case 'devicePaired': {
      const d = p.value.device
      if (!d) return next
      const via = p.value.pairingId ? { ...s.pairedVia, [d.id]: p.value.pairingId } : s.pairedVia
      return { ...next, pairedVia: via, devices: s.devices.some((x) => x.id === d.id) ? s.devices : [...s.devices, d] }
    }
    case 'deviceRevoked': {
      const { [p.value.deviceId]: _gone, ...pairedVia } = s.pairedVia
      return { ...next, pairedVia, devices: s.devices.filter((x) => x.id !== p.value.deviceId) }
    }
    default:
      return next
  }
}

function withoutPrompt(s: HostState, agentId: string, id: string): HostState {
  const list = s.prompts[agentId]
  return list?.some((x) => x.id === id) ? { ...s, prompts: { ...s.prompts, [agentId]: list.filter((x) => x.id !== id) } } : s
}

/** An agent's queue as the host listed it, which replaces what we hold for it. */
export function setPrompts(agentId: string, list: QueuedPrompt[]): (s: HostState) => Partial<HostState> {
  return (s) => ({ prompts: { ...s.prompts, [agentId]: list.filter((q) => !s.promptsDone[q.id]) } })
}

/** A prompt the host answered as queued, added before its event arrives (the event replaces it). */
export function addPrompt(q: QueuedPrompt): (s: HostState) => Partial<HostState> {
  return (s) => {
    const list = s.prompts[q.agentId] ?? []
    return list.some((x) => x.id === q.id) || s.promptsDone[q.id] ? {} : { prompts: { ...s.prompts, [q.agentId]: [...list, q] } }
  }
}

/** A prompt taken back, removed at once and for good (its event follows; an older listing doesn't bring it back). */
export function dropPrompt(agentId: string, id: string): (s: HostState) => Partial<HostState> {
  return (s) => {
    const list = s.prompts[agentId]
    return { promptsDone: { ...s.promptsDone, [id]: true }, ...(list?.some((x) => x.id === id) ? { prompts: { ...s.prompts, [agentId]: list.filter((x) => x.id !== id) } } : {}) }
  }
}

function withAgent(s: HostState, id: string, change: (a: Agent) => Agent): HostState {
  const a = s.agents[id]
  return a ? { ...s, agents: { ...s.agents, [id]: change(a) } } : s
}

/** Adds messages (a loaded page) to the ones we have; one we already have, which events may have updated, is kept. */
export function mergeMessages(s: HostState, page: Message[]): Partial<HostState> {
  const messages = { ...s.messages }
  for (const m of page) messages[m.id] ??= m
  return { messages }
}

/** Records how loading a run's messages stands. */
export function setMessageLoad(runId: string, state: MessageLoad | undefined): (s: HostState) => Partial<HostState> {
  return (s) => {
    if (s.messageLoad[runId] === state) return {}
    const { [runId]: _old, ...rest } = s.messageLoad
    return { messageLoad: state ? { ...rest, [runId]: state } : rest }
  }
}

/** The host's list of paired devices, which replaces the last one. */
export function setDevices(devices: Device[]): Partial<HostState> {
  return { devices }
}

/** The host's answer to ListWaiting, which replaces the last one. */
export function setWaiting(waiting: WaitingRow[]): Partial<HostState> {
  return { waiting }
}

/** The host's answer to ListPullRequests, which replaces the last one. */
export function setPullRequests(list: AgentPullRequests[]): Partial<HostState> {
  return { pullRequests: Object.fromEntries(list.map((x) => [x.agentId, x])) }
}

/** A fresh listing of settings replaces what we have (the schema can differ after a daemon update). */
export function setSettings(list: Setting[]): Partial<HostState> {
  return { settings: Object.fromEntries(list.map((x) => [x.key, x])) }
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
    onHello: (h: HelloResponse) => {
      const t = h.serverTime
      if (t) store.setState({ clockSkewMs: Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) - Date.now() })
    },
    cursor: () => store.getState().cursor,
    log: (text: string) =>
      store.setState((s) => ({ log: [...s.log.slice(-(MAX_LOG - 1)), { at: Date.now(), text }] })),
  }
}
