import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createClient, type Client } from '@connectrpc/connect'
import type { Transport } from '@connectrpc/connect'
import { MeshService, type SpawnAgentRequest } from './gen/agentmesh/v1/mesh_pb'
import { RequestMetaSchema, type AgentRole, type MessageType } from './gen/agentmesh/v1/types_pb'

export type MeshClient = Client<typeof MeshService>

export function createMeshClient(transport: Transport): MeshClient {
  return createClient(MeshService, transport)
}

/**
 * Every call the UI makes to a host, by registry method name. A test checks each is an action a device may call:
 * the UI can do nothing an agent can't do through the same registry.
 */
export const UI_ACTIONS = ['hello', 'startRun', 'listRuns', 'spawnAgent', 'listAgents', 'getAgent', 'stopAgent', 'sendMessage', 'subscribeEvents'] as const

/** A write's idempotency and expiry: a retried or delayed call neither runs twice nor runs late. */
export function meta(now = Date.now()) {
  return create(RequestMetaSchema, {
    mutationId: crypto.randomUUID(),
    expiresAt: timestampFromMs(now + 60_000),
  })
}

export const startRun = (c: MeshClient, title: string) => c.startRun({ meta: meta(), title })

export type SpawnInput = Pick<SpawnAgentRequest, 'runId' | 'repo' | 'slug' | 'kind' | 'model' | 'effort' | 'task'> & {
  role: AgentRole
}
export const spawnAgent = (c: MeshClient, input: SpawnInput) => c.spawnAgent({ meta: meta(), ...input })

export const stopAgent = (c: MeshClient, agent: string) => c.stopAgent({ meta: meta(), agent })

export const sendMessage = (c: MeshClient, to: string, type: MessageType, body: string) =>
  c.sendMessage({ meta: meta(), to, type, body })
