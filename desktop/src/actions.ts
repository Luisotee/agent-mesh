import { create, fromJson, type JsonValue } from '@bufbuild/protobuf'
import { ValueSchema, timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createClient, type Client } from '@connectrpc/connect'
import type { Transport } from '@connectrpc/connect'
import { MeshService, type SpawnAgentRequest } from './gen/agentmesh/v1/mesh_pb'
import { RequestMetaSchema, MessageType as MT, type AgentRole, type MessageType } from './gen/agentmesh/v1/types_pb'

export type MeshClient = Client<typeof MeshService>

export function createMeshClient(transport: Transport): MeshClient {
  return createClient(MeshService, transport)
}

/**
 * Every call the UI makes to a host, by registry method name. A test checks each is an action a device may call:
 * the UI can do nothing an agent can't do through the same registry.
 */
export const UI_ACTIONS = ['hello', 'startRun', 'listRuns', 'spawnAgent', 'listAgents', 'getAgent', 'stopAgent', 'sendMessage', 'subscribeEvents', 'openTerminal', 'listMessages', 'listWaiting', 'listSettings', 'setSetting', 'resetSetting', 'createPairing', 'listDevices', 'revokeDevice', 'tailnetStatus'] as const

/** A write's idempotency and expiry: a retried or delayed call neither runs twice nor runs late. */
export function meta(now = Date.now()) {
  return create(RequestMetaSchema, {
    mutationId: crypto.randomUUID(),
    expiresAt: timestampFromMs(now + 60_000),
  })
}

export const startRun = (c: MeshClient, title: string) => c.startRun({ meta: meta(), title })

export type SpawnInput = Pick<SpawnAgentRequest, 'runId' | 'repo' | 'slug' | 'kind' | 'tier' | 'model' | 'effort' | 'task'> & {
  role: AgentRole
}
export const spawnAgent = (c: MeshClient, input: SpawnInput) => c.spawnAgent({ meta: meta(), ...input })

export const stopAgent = (c: MeshClient, agent: string) => c.stopAgent({ meta: meta(), agent })

export const sendMessage = (c: MeshClient, to: string, type: MessageType, body: string) =>
  c.sendMessage({ meta: meta(), to, type, body })

/** A one-time ticket for an agent's terminal stream; `input` asks for the right to type (the host decides). */
export const openTerminal = (c: MeshClient, agent: string, input: boolean) => c.openTerminal({ agent, input })

/** One page of a run's messages, oldest first, after the message with `afterId` (empty: from the start). */
export const listMessages = (c: MeshClient, runId: string, afterId: string) => c.listMessages({ runId, afterId, limit: 500 })

/** The user's reply to what an agent sent: its plan approved, or a question answered, as an `answer` to its author. */
export const answer = (c: MeshClient, to: string, body: string) => sendMessage(c, to, MT.ANSWER, body)

/** What waits on the user across the host's open runs: plans and questions nobody answered, agents at a prompt. */
export const listWaiting = (c: MeshClient) => c.listWaiting({})

export const listSettings = (c: MeshClient, prefix = '') => c.listSettings({ prefix })

/** Set a setting to a JSON value; the host checks it against the setting's schema and says what's wrong. */
export const setSetting = (c: MeshClient, key: string, value: JsonValue) => c.setSetting({ meta: meta(), key, value: fromJson(ValueSchema, value) })

export const resetSetting = (c: MeshClient, key: string) => c.resetSetting({ meta: meta(), key })

/**
 * A one-time pairing token and its `agentmesh://pair` link, for a new device (a QR code and a link to copy). Only
 * the host's own device may ask: a paired device is refused.
 */
export const createPairing = (c: MeshClient, input: { name: string; readOnly: boolean; ttlSeconds: number; directUrl: string }) =>
  c.createPairing({ meta: meta(), name: input.name, readOnly: input.readOnly, ttlSeconds: BigInt(input.ttlSeconds), directUrl: input.directUrl })

export const listDevices = (c: MeshClient) => c.listDevices({ includeRevoked: false })

/** Ends a device's access: its token stops working and its open streams close. */
export const revokeDevice = (c: MeshClient, deviceId: string) => c.revokeDevice({ meta: meta(), deviceId })

/** The host's built-in Tailscale node (`meshd --tailscale`). Only the host's own device may ask: a paired device is refused. */
export const tailnetStatus = (c: MeshClient) => c.tailnetStatus({})
