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
export const UI_ACTIONS = ['hello', 'startRun', 'listRuns', 'spawnAgent', 'listAgents', 'getAgent', 'stopAgent', 'retireAgent', 'ringAgent', 'approveAgent', 'switchAgent', 'sendMessage', 'subscribeEvents', 'openTerminal', 'listMessages', 'readTranscript', 'promptAgent', 'listWaiting', 'listSettings', 'setSetting', 'resetSetting', 'createPairing', 'listDevices', 'revokeDevice', 'listPullRequests', 'getUsage', 'getCost', 'listProjects', 'holdPullRequest'] as const

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
  /** The branch or commit the new worktree starts from; empty: the repo's default branch. */
  base?: string
}
export const spawnAgent = (c: MeshClient, input: SpawnInput) => c.spawnAgent({ meta: meta(), ...input })

export const stopAgent = (c: MeshClient, agent: string) => c.stopAgent({ meta: meta(), agent })

/** Stops an agent's process on purpose and keeps its conversation: a message to it brings it back. */
export const retireAgent = (c: MeshClient, agent: string) => c.retireAgent({ meta: meta(), agent })

/** Rings an agent that has unread messages again (the host says why when it can't). */
export const ringAgent = (c: MeshClient, agent: string) => c.ringAgent({ meta: meta(), agent })

/** Approves an agent's plan: it goes on at its execution tier, in its own conversation. */
export const approveAgent = (c: MeshClient, agent: string) => c.approveAgent({ meta: meta(), agent })

/** Moves an agent to the next model of its chain (its model hit a quota), in its own conversation. */
export const switchToNextModel = (c: MeshClient, agent: string) => c.switchAgent({ meta: meta(), agent, next: true })

export const sendMessage = (c: MeshClient, to: string, type: MessageType, body: string) =>
  c.sendMessage({ meta: meta(), to, type, body })

/** A one-time ticket for an agent's terminal stream; `input` asks for the right to type (the host decides). */
export const openTerminal = (c: MeshClient, agent: string, input: boolean) => c.openTerminal({ agent, input })

/** One page of a run's messages, oldest first, after the message with `afterId` (empty: from the start). */
export const listMessages = (c: MeshClient, runId: string, afterId: string) => c.listMessages({ runId, afterId, limit: 500 })

/**
 * A page of an agent's conversation as its CLI recorded it: backward from `offset` (-1: the end), or on from it. The
 * host says `supported: false` for a CLI whose record it doesn't read.
 */
export const readTranscript = (c: MeshClient, agent: string, offset: bigint, backward: boolean) => c.readTranscript({ agent, offset, backward })

/** Types a prompt into the agent's running CLI as the user would (the chat composer): one process, one conversation. */
export const promptAgent = (c: MeshClient, agent: string, text: string) => c.promptAgent({ meta: meta(), agent, text })

/** The user's reply to what an agent sent: its plan approved, or a question answered, as an `answer` to its author. */
export const answer = (c: MeshClient, to: string, body: string) => sendMessage(c, to, MT.ANSWER, body)

/** What waits on the user across the host's open runs: plans and questions nobody answered, agents at a prompt. */
export const listWaiting = (c: MeshClient) => c.listWaiting({})

/** One run's change workers' pull requests with their state and checks, as GitHub was last read (the host caches it for two minutes). A device must name the run. */
export const listPullRequests = (c: MeshClient, runId: string, refresh = false) => c.listPullRequests({ runId, refresh })

/** Quota per vendor with the reserve line (the host reads it from the vendor, and caches it). */
export const getUsage = (c: MeshClient, refresh = false) => c.getUsage({ refresh })

/** What a run (or a window of time) cost, with the table per agent when `agents` is set. */
export const getCost = (c: MeshClient, scope: { runId: string } | { since: string }, agents = false) => c.getCost({ agents, runId: '', since: '', ...scope })

/** The repos the host can start work in (quick, from git); with `repo`, that one fetched, with its branches and open pull requests. */
export const listProjects = (c: MeshClient, repo = '') => c.listProjects({ repo })

/** Holds a pull request's merge, or releases it: a root never merges a held one. */
export const holdPullRequest = (c: MeshClient, url: string, release = false) => c.holdPullRequest({ meta: meta(), url, release })

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
