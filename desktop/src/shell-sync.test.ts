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
import { HelloResponseSchema, ListAgentsResponseSchema, ListMessagesResponseSchema, ListSettingsResponseSchema, ListWaitingResponseSchema, MeshService, SubscribeEventsResponseSchema, WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole, AgentSchema, AgentState, EventSchema, MessageSchema, MessageType, RunSchema, RunStatus, SettingSchema$ as SettingDescriptor, SettingSchemaSchema, type Event } from './gen/agentmesh/v1/types_pb'
import { createAppModel } from './model'

let dir: string
let server: http.Server
let push: (e: Event) => void
let settings: Record<string, Value>
let closeStreams: () => void
let settingsGate: Promise<void> | undefined
/** What the fake host answers to ListWaiting: a test sets it before the event that changes it, as the real host's rule would. */
let hostWaiting: ReturnType<typeof waiting>[]

const NOW = () => Date.now()
const agent = (id: string, name: string, role: AgentRole, parentId = '') => create(AgentSchema, { id, name, role, runId: 'r1', parentId, state: AgentState.IDLE })
const msg = (id: string, type: MessageType, from: string, to: string, body: string) =>
  create(MessageSchema, { id, runId: 'r1', type, body, toId: to, createdAt: timestampFromMs(NOW()), from: { $typeName: 'agentmesh.v1.Actor', id: from, name: from, class: ActorClass.WORKER, runId: 'r1' } })
const event = (seq: number, payload: Event['payload'], age = 100, agentId = 'w1') => create(EventSchema, { seq: BigInt(seq), generation: 'g', runId: 'r1', agentId, time: timestampFromMs(NOW() - age), payload })
const sentEvent = (seq: number, m: ReturnType<typeof msg>, age = 100) => event(seq, { case: 'messageSent', value: { $typeName: 'agentmesh.v1.MessageSent', message: m } }, age)
const waiting = (kind: WaitingKind, agentId: string, message?: ReturnType<typeof msg>) => create(WaitingSchema, { kind, runId: 'r1', agentId, message })
const settingOf = (key: string, type: string, def: unknown) =>
  create(SettingDescriptor, { key, value: settings[key] ?? fromJson(ValueSchema, def as never), default: fromJson(ValueSchema, def as never), isSet: key in settings, schema: create(SettingSchemaSchema, { type, summary: key }) })

beforeEach(async () => {
  dir = mkdtempSync(path.join(os.tmpdir(), 'amn-'))
  writeFileSync(path.join(dir, 'device.token'), 'tok')
  settings = {}
  settingsGate = undefined
  hostWaiting = []
  const queue: Event[] = []
  let wake: (() => void) | undefined
  let closed = false
  push = (e) => {
    queue.push(e)
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
          listWaiting: () => create(ListWaitingResponseSchema, { waiting: hostWaiting }),
          listAgents: () => create(ListAgentsResponseSchema, { agents: [agent('lead', 'app-lead', AgentRole.ROOT), agent('w1', 'app-fix', AgentRole.WORKER, 'lead')] }),
          listSettings: async () => {
            await settingsGate
            return create(ListSettingsResponseSchema, {
              settings: [settingOf('app.notify.enabled', 'bool', true), settingOf('app.notify.sound', 'bool', true), settingOf('app.notify.events', 'kinds', ['input', 'plan', 'question', 'report', 'failed']), settingOf('app.tray.close_to_tray', 'bool', false)],
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
  await vi.waitFor(() => expect(Object.keys(model.store.getState().hosts[0]!.store.getState().settings)).toHaveLength(4))
  return { model, shell, hostStore: model.store.getState().hosts[0]!.store }
}

it('turns live events into notifications by the host\'s own preferences, and counts what waits for the tray', async () => {
  const { shell } = await boot()
  expect(shell.configure).toHaveBeenLastCalledWith({ closeToTray: false }) // hiding to a tray is opt-in

  // History replayed after a reconnect (an old event) is silent; a live plan notifies, with the sound setting.
  const m0 = msg('m0', MessageType.PLAN, 'w1', 'lead', 'old plan')
  const m1 = msg('m1', MessageType.PLAN, 'w1', 'lead', '# The plan\n\n- do it')
  hostWaiting = [waiting(WaitingKind.PLAN, 'w1', m0), waiting(WaitingKind.PLAN, 'w1', m1)]
  push(sentEvent(1, m0, 3_600_000))
  push(sentEvent(2, m1))
  await vi.waitFor(() => expect(shell.notify).toHaveBeenCalledTimes(1))
  expect(shell.notify.mock.calls[0]![0]).toEqual({ tag: 'h:w1', title: 'app-fix sent a plan', body: 'The plan do it', sound: true, hostId: 'h', agentId: 'w1', tab: 'messages' })
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
  push(sentEvent(1, m1))
  await vi.waitFor(() => expect(hostStore.getState().messages['m1']).toBeDefined())
  expect(shell.notify).not.toHaveBeenCalled()

  // A needs-input prompt is a chosen kind.
  hostWaiting = [waiting(WaitingKind.PLAN, 'w1', m1), waiting(WaitingKind.INPUT, 'w1')]
  push(event(2, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.WORKING, to: AgentState.NEEDS_INPUT, detail: 'Bash', needsInput: 1 } }))
  await vi.waitFor(() => expect(shell.notify).toHaveBeenCalledTimes(1))
  expect(shell.notify.mock.calls[0]![0]).toMatchObject({ title: 'app-fix needs you', tab: 'terminal' })
  await vi.waitFor(() => expect(shell.setWaiting).toHaveBeenLastCalledWith(2)) // the plan and the prompt

  // Switched off, nothing notifies.
  hostStore.setState((s) => ({ settings: { ...s.settings, 'app.notify.enabled': { ...s.settings['app.notify.enabled']!, value: fromJson(ValueSchema, false) } } }))
  push(sentEvent(3, msg('m2', MessageType.QUESTION, 'w1', 'lead', 'a question')))
  await vi.waitFor(() => expect(hostStore.getState().messages['m2']).toBeDefined())
  expect(shell.notify).toHaveBeenCalledTimes(1)
})

it('follows a change of the tray setting, and a click on a notification opens the agent', async () => {
  const { model, shell, hostStore } = await boot()
  hostStore.setState((s) => ({ settings: { ...s.settings, 'app.tray.close_to_tray': { ...s.settings['app.tray.close_to_tray']!, value: fromJson(ValueSchema, true) } } }))
  expect(shell.configure).toHaveBeenLastCalledWith({ closeToTray: true })
  const click = shell.onNotificationClick.mock.calls[0]![0] as (c: { hostId: string; agentId: string; tab: string }) => void
  click({ hostId: 'h', agentId: 'w1', tab: 'messages' })
  expect(model.store.getState()).toMatchObject({ hostId: 'h', agentId: 'w1', tab: 'messages', settingsOpen: false })
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
  await vi.waitFor(() => expect(Object.keys(hostStore.getState().settings)).toHaveLength(4))
  expect(toJson(ValueSchema, hostStore.getState().settings['app.notify.enabled']!.value!)).toBe(false)
})
