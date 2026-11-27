// @vitest-environment node
import { create } from '@bufbuild/protobuf'
import { ValueSchema, timestampFromMs, type Value } from '@bufbuild/protobuf/wkt'
import { fromJson, toJson } from '@bufbuild/protobuf'
import { connectNodeAdapter } from '@connectrpc/connect-node'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { forward } from '../electron/bridge'
import type { StoredHost } from '../electron/hosts'
import type { DesktopApi } from '../shared/api'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { HelloResponseSchema, ListAgentsResponseSchema, ListMessagesResponseSchema, ListSettingsResponseSchema, ListShellsResponseSchema, ListWaitingResponseSchema, MeshService, ShellChangeSchema, ShellSchema, SubscribeEventsResponseSchema, WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole, AgentSchema, AgentState, EventSchema, MessageSchema, MessageType, RunSchema, RunStatus, SettingSchema$ as SettingDescriptor, SettingSchemaSchema, UserNoticeSchema, type Event } from './gen/agentmesh/v1/types_pb'
import { createAppModel } from './model'

let dir: string
let server: http.Server
let push: (e: Event) => void
/** A frame that is no event (a shell change), told to the app's stream. */
let pushFrame: (frame: ReturnType<typeof shellFrame>) => void
let settings: Record<string, Value>
let closeStreams: () => void
let settingsGate: Promise<void> | undefined
/** What the fake host answers to ListWaiting: a test sets it before the event that changes it, as the real host's rule would. */
let hostWaiting: ReturnType<typeof waiting>[]
/** The host's project shells, and a gate that holds a ListShells answer (computed when it was asked) on its way. */
let hostShells: string[]
let shellsGate: Promise<void> | undefined

const NOW = () => Date.now()
const shellFrame = (id: string, closed: boolean) => create(SubscribeEventsResponseSchema, { frame: { case: 'shell', value: create(ShellChangeSchema, { shell: create(ShellSchema, { id, repo: '/p/app' }), closed }) } })
const agent = (id: string, name: string, role: AgentRole, parentId = '') => create(AgentSchema, { id, name, role, runId: 'r1', parentId, state: AgentState.IDLE })
const msg = (id: string, type: MessageType, from: string, to: string, body: string) =>
  create(MessageSchema, { id, runId: 'r1', type, body, toId: to, createdAt: timestampFromMs(NOW()), from: { $typeName: 'agentmesh.v1.Actor', id: from, name: from, class: ActorClass.WORKER, runId: 'r1' } })
const event = (seq: number, payload: Event['payload'], age = 100, agentId = 'w1') => create(EventSchema, { seq: BigInt(seq), generation: 'g', runId: 'r1', agentId, time: timestampFromMs(NOW() - age), payload })
const sentEvent = (seq: number, m: ReturnType<typeof msg>, age = 100) => event(seq, { case: 'messageSent', value: { $typeName: 'agentmesh.v1.MessageSent', message: m } }, age)
const spawned = (seq: number, a: ReturnType<typeof agent>, age = 100, cls = ActorClass.WORKER) =>
  create(EventSchema, { seq: BigInt(seq), generation: 'g', runId: 'r1', agentId: a.id, time: timestampFromMs(NOW() - age), actor: { $typeName: 'agentmesh.v1.Actor', id: 'x', name: 'x', class: cls, runId: 'r1' }, payload: { case: 'agentSpawned', value: { $typeName: 'agentmesh.v1.AgentSpawned', agent: a } } })
const waiting = (kind: WaitingKind, agentId: string, message?: ReturnType<typeof msg>) => create(WaitingSchema, { kind, runId: 'r1', agentId, message, forUser: true })
/** What the host decided to tell the user (a `UserNotice` event). */
const noticed = (seq: number, kind: string, title: string, body: string, o: { age?: number; forUser?: boolean; tab?: string } = {}) =>
  event(seq, { case: 'userNotice', value: create(UserNoticeSchema, { kind, title, body, agentId: 'w1', tab: o.tab ?? 'chat', forUser: o.forUser ?? true, tag: 'w1' }) }, o.age ?? 100)
const settingOf = (key: string, type: string, def: unknown) =>
  create(SettingDescriptor, { key, value: settings[key] ?? fromJson(ValueSchema, def as never), default: fromJson(ValueSchema, def as never), isSet: key in settings, schema: create(SettingSchemaSchema, { type, summary: key }) })

beforeEach(async () => {
  dir = mkdtempSync(path.join(os.tmpdir(), 'amn-'))
  writeFileSync(path.join(dir, 'device.token'), 'tok')
  settings = {}
  settingsGate = undefined
  hostWaiting = []
  hostShells = ['a']
  shellsGate = undefined
  const queue: Event[] = []
  const frames: ReturnType<typeof shellFrame>[] = []
  let wake: (() => void) | undefined
  let closed = false
  push = (e) => {
    queue.push(e)
    wake?.()
  }
  pushFrame = (f) => {
    frames.push(f)
    wake?.()
  }
  closeStreams = () => {
    closed = true
    wake?.()
  }
  server = http.createServer(
    connectNodeAdapter({
      routes: (router) =>
        router.service(MeshService, {
          hello: () => create(HelloResponseSchema, { daemonId: 'd', instanceId: 'i', version: 't', capabilities: [] }),
          listMessages: () => create(ListMessagesResponseSchema, { messages: [] }),
          listShells: async () => {
            const asked = hostShells.map((id) => create(ShellSchema, { id, repo: '/p/app' })) // the host's shells when it was asked
            await shellsGate
            return create(ListShellsResponseSchema, { shells: asked })
          },
          listWaiting: () => create(ListWaitingResponseSchema, { waiting: hostWaiting }),
          listAgents: () => create(ListAgentsResponseSchema, { agents: [agent('lead', 'app-lead', AgentRole.ROOT), agent('w1', 'app-fix', AgentRole.WORKER, 'lead')] }),
          listSettings: async () => {
            await settingsGate
            return create(ListSettingsResponseSchema, {
              settings: [settingOf('app.notify.enabled', 'bool', true), settingOf('app.notify.sound', 'bool', true), settingOf('app.notify.events', 'kinds', ['input', 'plan', 'question', 'report', 'failed', 'error', 'finished', 'ready']), settingOf('app.tray.close_to_tray', 'bool', false), settingOf('app.dock.new_agents', 'string', 'dock')],
            })
          },
          async *subscribeEvents() {
            yield create(SubscribeEventsResponseSchema, {
              frame: {
                case: 'snapshot',
                value: {
                  cursor: { generation: 'g', seq: 0n },
                  runs: [create(RunSchema, { id: 'r1', title: 'run', status: RunStatus.OPEN })],
                  agents: [agent('lead', 'app-lead', AgentRole.ROOT), agent('w1', 'app-fix', AgentRole.WORKER, 'lead')],
                },
              },
            })
            while (!closed) {
              while (queue.length) yield create(SubscribeEventsResponseSchema, { frame: { case: 'event', value: queue.shift()! } })
              while (frames.length) yield frames.shift()!
              await new Promise<void>((r) => (wake = r))
            }
          },
        }),
    }),
  )
  await new Promise<void>((r) => server.listen(path.join(dir, 'meshd.sock'), r))
})

afterEach(async () => {
  closeStreams()
  server.closeAllConnections()
  await new Promise((r) => server.close(r))
  rmSync(dir, { recursive: true, force: true })
})

async function boot() {
  const host: StoredHost = { id: 'h', name: 'home', kind: 'state-dir', path: dir }
  const shell = { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn((_cb: unknown) => () => {}) }
  const api = {
    hosts: { list: async () => [{ id: 'h', name: 'home', kind: 'state-dir' as const, target: dir }], fixed: true },
    request: (_id: string, req: Parameters<typeof forward>[1], cb: Parameters<typeof forward>[2]) => forward(host, req, cb),
    openSocket: vi.fn(),
    shell,
  } as unknown as DesktopApi
  const model = createAppModel(api)
  await model.load()
  await vi.waitFor(() => expect(Object.keys(model.store.getState().hosts[0]!.store.getState().settings)).toHaveLength(5))
  return { model, shell, hostStore: model.store.getState().hosts[0]!.store }
}

it('turns live events into notifications by the host\'s own preferences, and counts what waits for the tray', async () => {
  const { shell } = await boot()
  expect(shell.configure).toHaveBeenLastCalledWith({ closeToTray: false }) // hiding to a tray is opt-in

  // History replayed after a reconnect (an old event) is silent; a live plan notifies, with the sound setting.
  const m0 = msg('m0', MessageType.PLAN, 'w1', 'lead', 'old plan')
  const m1 = msg('m1', MessageType.PLAN, 'w1', 'lead', '# The plan\n\n- do it')
  hostWaiting = [waiting(WaitingKind.PLAN, 'w1', m0), waiting(WaitingKind.PLAN, 'w1', m1)]
  push(sentEvent(10, m0, 3_600_000))
  push(sentEvent(11, m1))
  push(noticed(1, 'plan', 'app-fix sent a plan', 'old plan', { age: 3_600_000 }))
  push(noticed(2, 'plan', 'app-fix sent a plan', 'The plan do it'))
  await vi.waitFor(() => expect(shell.notify).toHaveBeenCalledTimes(1))
  expect(shell.notify.mock.calls[0]![0]).toEqual({ tag: 'h:w1', title: 'app-fix sent a plan', body: 'The plan do it', sound: true, hostId: 'h', agentId: 'w1', tab: 'chat' })
  // Both plans wait (the old one is still unanswered), counted for the tray.
  await vi.waitFor(() => expect(shell.setWaiting).toHaveBeenLastCalledWith(2))

  // An answer closes them: the host says nothing waits any more.
  hostWaiting = []
  push(event(3, { case: 'messageSent', value: { $typeName: 'agentmesh.v1.MessageSent', message: { ...msg('a1', MessageType.ANSWER, 'lead', 'w1', 'approved'), from: { $typeName: 'agentmesh.v1.Actor', id: 'dev', name: 'dev', class: ActorClass.DEVICE, runId: '' } } } }))
  await vi.waitFor(() => expect(shell.setWaiting).toHaveBeenLastCalledWith(0))
  expect(shell.notify).toHaveBeenCalledTimes(1)
})

it('stays quiet when notifications are off or the kind is not chosen', async () => {
  settings['app.notify.events'] = fromJson(ValueSchema, ['input'])
  const { shell, hostStore } = await boot()
  const m1 = msg('m1', MessageType.PLAN, 'w1', 'lead', 'a plan')
  hostWaiting = [waiting(WaitingKind.PLAN, 'w1', m1)]
  push(sentEvent(10, m1))
  push(noticed(1, 'plan', 'app-fix sent a plan', 'a plan'))
  await vi.waitFor(() => expect(hostStore.getState().activity).toHaveLength(1))
  expect(shell.notify).not.toHaveBeenCalled()

  // A notice for Activity alone never notifies, even of a chosen kind; a needs-input prompt is a chosen kind.
  push(noticed(2, 'input', 'app-fix needs you', 'Bash', { forUser: false, tab: 'terminal' }))
  await vi.waitFor(() => expect(hostStore.getState().activity).toHaveLength(2))
  expect(shell.notify).not.toHaveBeenCalled()
  hostWaiting = [waiting(WaitingKind.PLAN, 'w1', m1), waiting(WaitingKind.INPUT, 'w1')]
  push(event(12, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.WORKING, to: AgentState.NEEDS_INPUT, detail: 'Bash', needsInput: 1 } }))
  push(noticed(3, 'input', 'app-fix needs you', 'Bash', { tab: 'terminal' }))
  await vi.waitFor(() => expect(shell.notify).toHaveBeenCalledTimes(1))
  expect(shell.notify.mock.calls[0]![0]).toMatchObject({ title: 'app-fix needs you', tab: 'terminal' })
  await vi.waitFor(() => expect(shell.setWaiting).toHaveBeenLastCalledWith(2)) // the plan and the prompt

  // Switched off, nothing notifies.
  hostStore.setState((s) => ({ settings: { ...s.settings, 'app.notify.enabled': { ...s.settings['app.notify.enabled']!, value: fromJson(ValueSchema, false) } } }))
  push(noticed(4, 'input', 'app-fix needs you again', 'Bash', { tab: 'terminal' }))
  await vi.waitFor(() => expect(hostStore.getState().activity).toHaveLength(4))
  expect(shell.notify).toHaveBeenCalledTimes(1)
})

it('follows a change of the tray setting, and a click on a notification opens the agent', async () => {
  const { model, shell, hostStore } = await boot()
  hostStore.setState((s) => ({ settings: { ...s.settings, 'app.tray.close_to_tray': { ...s.settings['app.tray.close_to_tray']!, value: fromJson(ValueSchema, true) } } }))
  expect(shell.configure).toHaveBeenLastCalledWith({ closeToTray: true })
  const click = shell.onNotificationClick.mock.calls[0]![0] as (c: { hostId: string; agentId: string; tab: string }) => void
  click({ hostId: 'h', agentId: 'w1', tab: 'chat' })
  expect(model.store.getState()).toMatchObject({ hostId: 'h', agentId: 'w1', tab: 'chat', settingsOpen: false })
  // A click on a notification of a host that isn't there any more goes nowhere.
  click({ hostId: 'gone', agentId: 'x', tab: 'terminal' })
  expect(model.store.getState()).toMatchObject({ hostId: 'h', agentId: 'w1' })
})

it('a setting changed while the list is on its way is not lost to it', async () => {
  let open!: () => void
  settingsGate = new Promise<void>((r) => (open = r))
  const host: StoredHost = { id: 'h', name: 'home', kind: 'state-dir', path: dir }
  const shell = { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn((_cb: unknown) => () => {}) }
  const api = {
    hosts: { list: async () => [{ id: 'h', name: 'home', kind: 'state-dir' as const, target: dir }], fixed: true },
    request: (_id: string, req: Parameters<typeof forward>[1], cb: Parameters<typeof forward>[2]) => forward(host, req, cb),
    openSocket: vi.fn(),
    shell,
  } as unknown as DesktopApi
  const model = createAppModel(api)
  await model.load()
  const hostStore = model.store.getState().hosts[0]!.store
  await vi.waitFor(() => expect(hostStore.getState().synced).toBe(true))
  // Someone turns notifications off while the host's list (computed before that) is still in flight.
  push(event(1, { case: 'settingChanged', value: { $typeName: 'agentmesh.v1.SettingChanged', key: 'app.notify.enabled', value: fromJson(ValueSchema, false), toDefault: false } }))
  await vi.waitFor(() => expect(hostStore.getState().cursor?.seq).toBe(1n))
  open()
  await vi.waitFor(() => expect(Object.keys(hostStore.getState().settings)).toHaveLength(5))
  expect(toJson(ValueSchema, hostStore.getState().settings['app.notify.enabled']!.value!)).toBe(false)
})

it('marks a worker that starts while the app is open as new in the dock, without moving focus, and only that worker', async () => {
  const { model, hostStore } = await boot()
  model.selectAgent('h', 'lead')
  // Live and started by an agent: marked new. History (an hour old) and the user's own spawn aren't.
  push(spawned(1, agent('w2', 'app-new', AgentRole.WORKER, 'lead')))
  push(spawned(2, agent('w3', 'app-old', AgentRole.WORKER, 'lead'), 3_600_000))
  push(spawned(3, agent('w4', 'app-mine', AgentRole.WORKER, 'lead'), 100, ActorClass.DEVICE))
  await vi.waitFor(() => expect(Object.keys(hostStore.getState().agents)).toContain('w4'))
  const s = model.store.getState()
  expect(Object.keys(s.newTiles)).toEqual(['h/w2'])
  expect(s).toMatchObject({ agentId: 'lead', tabs: [{ hostId: 'h', agentId: 'lead' }], dockBadge: 0 })
  // Looking at it clears the mark.
  model.selectAgent('h', 'w2')
  expect(model.store.getState().newTiles).toEqual({})
})

it('opens a new worker as a background tab when the host says tab', async () => {
  settings['app.dock.new_agents'] = fromJson(ValueSchema, 'tab')
  const { model, hostStore } = await boot()
  model.selectAgent('h', 'lead')
  push(spawned(1, agent('w2', 'app-new', AgentRole.WORKER, 'lead')))
  await vi.waitFor(() => expect(Object.keys(hostStore.getState().agents)).toContain('w2'))
  expect(model.store.getState()).toMatchObject({ agentId: 'lead', tabs: [{ agentId: 'lead' }, { agentId: 'w2' }], newTiles: {} })
})

it('counts a new worker on the dock\'s toggle while the dock is hidden, and clears the count when it is shown', async () => {
  const { model, hostStore } = await boot()
  model.setDock(false)
  push(spawned(1, agent('w5', 'app-late', AgentRole.WORKER, 'lead')))
  await vi.waitFor(() => expect(Object.keys(hostStore.getState().agents)).toContain('w5'))
  expect(model.store.getState()).toMatchObject({ dockBadge: 1, dockOpen: false })
  model.setDock(true)
  expect(model.store.getState().dockBadge).toBe(0)
})

it('a dismissed dock tile is spent once its agent leaves the state, so it shows again when the agent returns to it', async () => {
  const { model, hostStore } = await boot()
  const stateOf = (to: AgentState) => event(1, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.IDLE, to, detail: '', needsInput: to === AgentState.NEEDS_INPUT ? 1 : 0 } })
  const w1 = hostStore.getState().agents.w1!.state
  model.dismissTile('h', 'w1')
  expect(model.store.getState().dismissed).toEqual({ 'h/w1': w1 })
  push(stateOf(AgentState.NEEDS_INPUT))
  await vi.waitFor(() => expect(hostStore.getState().agents.w1!.state).toBe(AgentState.NEEDS_INPUT))
  expect(model.store.getState().dismissed).toEqual({})
})

it('follows a shell opening and ending on the host without listing again', async () => {
  const { model } = await boot()
  const ids = () => (model.store.getState().shells['h'] ?? []).map((x) => x.id)
  await vi.waitFor(() => expect(ids()).toEqual(['a'])) // listed once, when the host connected
  pushFrame(shellFrame('b', false))
  await vi.waitFor(() => expect(ids()).toEqual(['a', 'b']))
  pushFrame(shellFrame('a', true))
  await vi.waitFor(() => expect(ids()).toEqual(['b']))
})

it('a shell opened while the list is on its way is not lost to it', async () => {
  let open!: () => void
  shellsGate = new Promise<void>((r) => (open = r))
  const { model } = await boot()
  const ids = () => (model.store.getState().shells['h'] ?? []).map((x) => x.id)
  // The host read its shells ('a' only) before 'b' opened; the frame comes before the answer does, and finds no list to join.
  hostShells = ['a', 'b']
  pushFrame(shellFrame('b', false))
  await new Promise((r) => setTimeout(r, 100))
  expect(model.store.getState().shells['h']).toBeUndefined()
  open()
  await vi.waitFor(() => expect(ids()).toEqual(['a', 'b']))
})
