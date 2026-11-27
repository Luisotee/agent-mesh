import { create, fromJson, type JsonValue } from '@bufbuild/protobuf'
import { ValueSchema, timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createClient, type Client } from '@connectrpc/connect'
import type { Transport } from '@connectrpc/connect'
import { MeshService, type SpawnAgentRequest } from './gen/agentmesh/v1/mesh_pb'
import { RequestMetaSchema, MessageType as MT, type AgentRole, type MessageType, type RunAutonomy } from './gen/agentmesh/v1/types_pb'

export type MeshClient = Client<typeof MeshService>

export function createMeshClient(transport: Transport): MeshClient {
  return createClient(MeshService, transport)
}

/**
 * Every call the UI makes to a host, by registry method name. A test checks each is an action a device may call:
 * the UI can do nothing an agent can't do through the same registry.
 */
export const UI_ACTIONS = ['hello', 'startRun', 'listRuns', 'spawnAgent', 'listAgents', 'getAgent', 'stopAgent', 'retireAgent', 'stopRun', 'ringAgent', 'approveAgent', 'switchAgent', 'sendMessage', 'subscribeEvents', 'openTerminal', 'listMessages', 'readTranscript', 'listAgentCommands', 'getPromptSuggestion', 'promptAgent', 'listPrompts', 'withdrawPrompt', 'sendPromptAnyway', 'interruptAgent', 'rewindAgent', 'addAttachment', 'listWaiting', 'listSettings', 'setSetting', 'resetSetting', 'createPairing', 'listDevices', 'revokeDevice', 'listPullRequests', 'getUsage', 'getCost', 'listProjects', 'holdPullRequest', 'checkModels', 'tailnetStatus', 'startSetup', 'renameRun', 'openView', 'listPermissions', 'decidePermission', 'getAgentChanges', 'getAgentProgress', 'archiveAgent', 'resumeAgent', 'openInEditor', 'setTailnet', 'openShell', 'listShells', 'closeShell'] as const

/** A write's idempotency and expiry: a retried or delayed call neither runs twice nor runs late. */
export function meta(now = Date.now()) {
  return create(RequestMetaSchema, {
    mutationId: crypto.randomUUID(),
    expiresAt: timestampFromMs(now + 60_000),
  })
}

/** Starts a run; `autonomy` is what the user lets its root do without asking, for this run alone (unset: the host's settings apply). */
export const startRun = (c: MeshClient, title: string, autonomy?: Pick<RunAutonomy, 'userApprovesPlans' | 'userMerges' | 'askBefore' | 'keepRepoLists'>) =>
  c.startRun({ meta: meta(), title, autonomy: autonomy ? { userApprovesPlans: autonomy.userApprovesPlans, userMerges: autonomy.userMerges, askBefore: autonomy.askBefore, keepRepoLists: autonomy.keepRepoLists } : undefined })

/** Changes a run's title. */
export const renameRun = (c: MeshClient, runId: string, title: string) => c.renameRun({ meta: meta(), runId, title })

/** Asks the user's apps to show a view (`agent`, `terminal`, `messages`, `plan`, `pr`, `settings` or `run`): the same call an agent's CLI makes. */
export const openView = (c: MeshClient, view: 'agent' | 'terminal' | 'settings' | 'run' | 'messages' | 'plan' | 'pr', target = '', runId = '') => c.openView({ meta: meta(), view, target, runId })

/** The permission prompts held for agents' parents now (the host keeps them in memory only). */
export const listPermissions = (c: MeshClient) => c.listPermissions({})

/** Answers a held permission prompt: `allow`, or `deny` with the reason the agent reads. */
export const decidePermission = (c: MeshClient, id: string, decision: 'allow' | 'deny', why = '', extra: { answers?: Array<{ question: string; labels: string[]; other: string }>; mode?: string } = {}) =>
  c.decidePermission({ meta: meta(), id, decision, why, answers: extra.answers ?? [], mode: extra.mode ?? '' })

export type SpawnInput = Pick<SpawnAgentRequest, 'runId' | 'repo' | 'slug' | 'kind' | 'tier' | 'model' | 'effort' | 'task'> & {
  role: AgentRole
  /** The branch or commit the new worktree starts from; empty: the repo's default branch. */
  base?: string
  /** A plain agent: the user's own conversation with the CLI, a root with no task that the mesh never resets or briefs. */
  plain?: boolean
}
export const spawnAgent = (c: MeshClient, input: SpawnInput) => c.spawnAgent({ meta: meta(), ...input })

/**
 * Starts the setup agent, or lands on the one that runs: a lead agent in a run of its own that helps connect repos,
 * machines and devices (the host's `protocol/SETUP.md`). Asked again it returns the same agent.
 */
export const startSetup = (c: MeshClient) => c.startSetup({ meta: meta() })

export const stopAgent = (c: MeshClient, agent: string) => c.stopAgent({ meta: meta(), agent })

/** Stops an agent's process on purpose and keeps its conversation: a task or an answer to it brings it back. */
export const retireAgent = (c: MeshClient, agent: string) => c.retireAgent({ meta: meta(), agent })

/** Stops a run: every agent in it is retired and the run is closed; a clean worktree goes, one with changes stays. */
export const stopRun = (c: MeshClient, runId: string) => c.stopRun({ meta: meta(), runId })

/** Rings an agent that has unread messages again (the host says why when it can't). */
export const ringAgent = (c: MeshClient, agent: string) => c.ringAgent({ meta: meta(), agent })

/** Approves an agent's plan: it goes on at its execution tier, in its own conversation. */
export const approveAgent = (c: MeshClient, agent: string) => c.approveAgent({ meta: meta(), agent })

/**
 * Moves an agent to another tier, model or effort, in its own conversation (the host reopens it: `mesh switch`). The host
 * refuses what it can't do and says why (already on it, at the quota reserve, a free model of a repo that wants none).
 */
export const switchAgent = (c: MeshClient, agent: string, to: { tier?: string; model?: string; effort?: string }) =>
  c.switchAgent({ meta: meta(), agent, tier: to.tier ?? '', model: to.model ?? '', effort: to.effort ?? '', next: false, why: '' })

/** The model families each CLI lists (the host reuses a check up to a day old). */
export const checkModels = (c: MeshClient) => c.checkModels({ meta: meta(), check: false, ack: [] })

/** Moves an agent to the next model of its chain (its model hit a quota), in its own conversation. */
export const switchToNextModel = (c: MeshClient, agent: string) => c.switchAgent({ meta: meta(), agent, next: true })

export const sendMessage = (c: MeshClient, to: string, type: MessageType, body: string) =>
  c.sendMessage({ meta: meta(), to, type, body })

/** A one-time ticket for an agent's terminal stream; `input` asks for the right to type (the host decides). */
export const openTerminal = (c: MeshClient, agent: string, input: boolean) => c.openTerminal({ agent, input })

/** A project shell: a login shell in the project's folder on the host. `repo` must be a project the host lists. */
export const openShell = (c: MeshClient, repo: string) => c.openShell({ meta: meta(), repo })
export const listShells = (c: MeshClient) => c.listShells({})
/** Ends the shell (the host stops it and forgets it). */
export const closeShell = (c: MeshClient, id: string) => c.closeShell({ meta: meta(), id })
/** A one-time ticket for a shell's terminal stream, the same stream as an agent's. */
export const openShellTerminal = (c: MeshClient, shell: string, input: boolean) => c.openTerminal({ shell, input })

/** One page of a run's messages, oldest first, after the message with `afterId` (empty: from the start). */
export const listMessages = (c: MeshClient, runId: string, afterId: string) => c.listMessages({ runId, afterId, limit: 500 })

/**
 * A page of an agent's conversation as its CLI recorded it: backward from `offset` (-1: the end), or on from it. The
 * host says `supported: false` for a CLI whose record it doesn't read.
 */
export const readTranscript = (c: MeshClient, agent: string, offset: bigint, backward: boolean) => c.readTranscript({ agent, offset, backward })

/** The commands the agent's CLI accepts at its prompt, with descriptions and argument hints, for the composer's `/` completion. */
export const listAgentCommands = (c: MeshClient, agent: string) => c.listAgentCommands({ agent })

/** The prompt Claude Code suggests next (grey text in its empty box); empty when there is none or the CLI has no such feature. */
export const getPromptSuggestion = (c: MeshClient, agent: string) => c.getPromptSuggestion({ agent })

/** Types a prompt into the agent's running CLI as the user would (the chat composer): one process, one conversation. */
export const promptAgent = (c: MeshClient, agent: string, text: string) => c.promptAgent({ meta: meta(), agent, text })

/** The prompts sent to an agent's chat that wait to be typed at its next idle, oldest first, with what they wait for. */
export const listPrompts = (c: MeshClient, agent: string) => c.listPrompts({ agent })

/** Takes back a queued prompt before it is typed (the newest when `promptId` is empty); refused once it is being typed. */
export const withdrawPrompt = (c: MeshClient, agent: string, promptId = '') => c.withdrawPrompt({ meta: meta(), agent, promptId })

/**
 * Types a held queued prompt now over the draft in the agent's input box: `draft` is the text the user was shown
 * (`QueuedPrompt.heldText`) and confirmed; the host refuses when the box holds anything else by now, when someone is typing in
 * the terminal, or when the agent is not at its prompt.
 */
export const sendPromptAnyway = (c: MeshClient, agent: string, promptId: string, draft: string) => c.sendPromptAnyway({ meta: meta(), agent, promptId, draft })

/** Interrupts an agent's turn, as Esc does in its terminal. */
export const interruptAgent = (c: MeshClient, agent: string) => c.interruptAgent({ meta: meta(), agent })

/** Takes an agent's conversation back to before one of its prompts (a transcript entry's id); its text comes back. */
export const rewindAgent = (c: MeshClient, agent: string, entryId: string) => c.rewindAgent({ meta: meta(), agent, entryId })

/** Stores an image on the agent's host for a prompt; the path it returns goes in the prompt. Its expiry is long: the host checks it after the whole image arrived. */
export const addAttachment = (c: MeshClient, agent: string, name: string, data: Uint8Array) => c.addAttachment({ meta: meta(Date.now() + 9 * 60_000), agent, name, data })

/** The user's reply to what an agent sent: its plan approved, or a question answered, as an `answer` to its author. */
export const answer = (c: MeshClient, to: string, body: string) => sendMessage(c, to, MT.ANSWER, body)

/** What waits on the user across the host's open runs: plans and questions nobody answered, agents at a prompt. */
/** What an agent's worktree holds that no remote has: the files changed since its branch started, uncommitted work, commits not pushed. */
/** `withIgnored` also lists the git-ignored files archiving would delete (a walk of the worktree: for the Archive dialog, not the side panel's poll). */
export const getAgentChanges = (c: MeshClient, agent: string, withIgnored = false) => c.getAgentChanges({ agent, withIgnored })

/** An agent's plan (its todo list) and how much context its conversation holds, from its CLI's record (Claude Code only). */
export const getAgentProgress = (c: MeshClient, agent: string) => c.getAgentProgress({ agent })

/**
 * Retires an agent and removes its worktree, keeping its branch. The host refuses while the worktree has uncommitted
 * changes, and (without `force`) while the branch has commits no remote branch has.
 */
export const archiveAgent = (c: MeshClient, agent: string, force = false) => c.archiveAgent({ meta: meta(), agent, force })

/** Brings a stopped, retired or archived agent back as it was: an archived agent's worktree is made again from its branch. */
export const resumeAgent = (c: MeshClient, agent: string) => c.resumeAgent({ meta: meta(), agent })

/** Opens a path of an agent's worktree (empty: the worktree) in the host's editor. Only the host's own device may: it runs a program on the host. */
export const openInEditor = (c: MeshClient, agent: string, path = '') => c.openInEditor({ meta: meta(), agent, path })

/** Turns the host's Tailscale node on or off; meshd restarts for it to take effect (itself, under a service manager). Only the host's own device may. */
export const setTailnet = (c: MeshClient, enabled: boolean) => c.setTailnet({ meta: meta(), enabled })

/** One agent, with the models it ran on (its spawn and each reopen that moved it). */
export const getAgent = (c: MeshClient, agent: string) => c.getAgent({ agent })

export const listWaiting = (c: MeshClient) => c.listWaiting({})

/** One run's change workers' pull requests with their state and checks, as GitHub was last read (the host caches it for two minutes). A device must name the run. */
export const listPullRequests = (c: MeshClient, runId: string, refresh = false) => c.listPullRequests({ runId, refresh })

/** The repos the host can start work in (quick, from git); with `repo`, that one fetched, with its branches and open pull requests. */
export const listProjects = (c: MeshClient, repo = '', quick = false) => c.listProjects(quick ? { repo, quick } : { repo })

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

/** The host's built-in Tailscale node (`meshd --tailscale`). Only the host's own device may ask: a paired device is refused. */
export const tailnetStatus = (c: MeshClient) => c.tailnetStatus({})

/** Quota usage (Claude's windows, agy's rows and tiers) and the reserve line, the same as `mesh usage`; `refresh` skips the host's cache. */
export const getUsage = (c: MeshClient, refresh = false) => c.getUsage({ refresh })

/**
 * Spend from the session files, the same as `mesh cost`. A device names a run, or a window back from now (`24h`, `7d`):
 * the host refuses a request that names neither.
 */
export const getCost = (c: MeshClient, scope: { runId: string } | { since: string }, agents = false) => c.getCost({ agents, runId: '', since: '', ...scope })
