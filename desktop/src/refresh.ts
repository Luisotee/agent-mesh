import { equals } from '@bufbuild/protobuf'
import type { MeshClient } from './actions'
import { listAgentWaits, listPermissions, listPullRequests, listWaiting } from './actions'
import { realTimers, type Timers } from './connection'
import { AgentSchema, MessageType, RunStatus, type Agent, type Event } from './gen/agentmesh/v1/types_pb'
import { setAgentWaits, setGithubProblem, setPermissions, setPullRequests, setWaiting, type HostStore } from './store'

/**
 * Three things the event stream doesn't carry, read from the host again when they may have changed.
 *
 * What waits on the user is the host's to say (`ListWaiting`), so it is listed again, after a short pause that
 * folds a burst of events into one call, whenever an event could change it: a plan, question or answer, an agent
 * starting, changing state, exiting or being retired, a run stopping. A list that was on its way while one of those
 * arrived may be stale: it is shown, and fetched again after the pause.
 *
 * What each idle agent waits on (`ListAgentWaits`: its background job, a wait it declared, an answer owed) is read on the same
 * pause, and every `waitsEveryMs`: a job ends and a deadline passes with no event.
 *
 * The pull requests (`ListPullRequests`, which the host caches for two minutes) are read at connect and every
 * `prsEveryMs`; a PR that changes between is told by an event the store applies itself.
 *
 * An agent that went quiet shows as `unknown` only when the host is asked (the state is derived from the time and
 * the terminal's output when read, never stored or sent as an event), so the agents are listed again every
 * `agentsEveryMs`; an agent an event touched meanwhile keeps what the event said.
 */
export function hostRefresher(
  client: Pick<MeshClient, 'listWaiting' | 'listAgentWaits' | 'listAgents' | 'listPullRequests' | 'listPermissions'>,
  store: HostStore,
  log: (line: string) => void,
  timers: Timers = realTimers,
  opts: { debounceMs?: number; agentsEveryMs?: number; prsEveryMs?: number; waitsEveryMs?: number } = {},
) {
  const debounceMs = opts.debounceMs ?? 200
  const agentsEveryMs = opts.agentsEveryMs ?? 30_000
  const prsEveryMs = opts.prsEveryMs ?? 60_000
  const waitsEveryMs = opts.waitsEveryMs ?? 15_000
  let stopped = false
  let waitingTouches = 0
  let agentTouches = 0
  let pause: unknown
  let waitsPause: unknown
  let waitingLoading = false
  let ticker: unknown
  let prTicker: unknown
  let waitsTicker: unknown
  let waitsLoading = false
  let waitsAgain = false
  let waitsComplained = false
  let prsLoading = false
  let prsComplained = false
  // PR events the store applied: a read that was under way when one came is older than it, and must not undo it.
  let prTouches = 0
  let complained = false
  let waitingFailed = false
  // The held permission prompts: listed at connect and when the host says one was held or ended.
  let permLoading = false
  let permAgain = false
  let permComplained = false

  const complain = (what: string, e: unknown) => {
    // A host that doesn't know the action (an older daemon) would say so every 30 s: once is enough.
    if (complained) return
    complained = true
    log(`couldn't ${what}: ${e instanceof Error ? e.message : String(e)}`)
  }

  async function loadWaiting(): Promise<void> {
    if (waitingLoading) {
      // One call at a time; what changed meanwhile is asked for after the pause.
      schedule()
      return
    }
    waitingLoading = true
    try {
      const seen = waitingTouches
      const res = await listWaiting(client as MeshClient)
      if (stopped) return
      waitingFailed = false
      complained = false
      // Shown even if an event overtook it (a busy host answers slower than events come, and a list never shown would
      // leave the old one up for as long as the burst lasts); the event scheduled a call, or this one does, to correct it.
      // Only what the user is to act on: a worker's plan to a lead that decides it is the lead's (and in Activity).
      store.setState(setWaiting(res.waiting, store.getState().waitingAll))
      if (seen !== waitingTouches) schedule()
    } catch (e) {
      // The list stays as it was; the next tick asks again.
      waitingFailed = true
      complain('list what waits on you', e)
    } finally {
      waitingLoading = false
    }
  }

  async function loadAgentWaits(): Promise<void> {
    const st = store.getState()
    if (st.conn.kind !== 'ready' || !st.synced) return
    if (waitsLoading) {
      waitsAgain = true // what changed meanwhile is asked for once this call is done
      return
    }
    waitsLoading = true
    try {
      const res = await listAgentWaits(client as MeshClient)
      if (stopped) return
      waitsComplained = false
      store.setState(setAgentWaits(res.waits, store.getState().agentWaits))
    } catch (e) {
      // An older host doesn't know the action; said once. The words stay as they were.
      if (!waitsComplained) log(`couldn't list what the idle agents wait on: ${e instanceof Error ? e.message : String(e)}`)
      waitsComplained = true
    } finally {
      waitsLoading = false
      if (waitsAgain && !stopped) {
        waitsAgain = false
        void loadAgentWaits()
      }
    }
  }

  async function loadPermissions(): Promise<void> {
    if (permLoading) {
      permAgain = true // what changed meanwhile is asked for once this call is done
      return
    }
    permLoading = true
    try {
      const res = await listPermissions(client as MeshClient)
      if (stopped) return
      permComplained = false
      store.setState(setPermissions(res.permissions))
    } catch (e) {
      // An older host doesn't know the action; said once. The list stays as it was.
      if (!permComplained) log(`couldn't list the permission prompts: ${e instanceof Error ? e.message : String(e)}`)
      permComplained = true
    } finally {
      permLoading = false
      if (permAgain && !stopped) {
        permAgain = false
        void loadPermissions()
      }
    }
  }

  /** At most one call per `debounceMs`, however many events come: a burst is one call, and a steady stream can't starve it. */
  const schedule = () => {
    if (stopped || pause !== undefined) return
    // This pause reads the idle agents' words too: one for the burst, not one more from `scheduleWaits`.
    timers.clearTimeout(waitsPause)
    waitsPause = undefined
    pause = timers.setTimeout(() => {
      pause = undefined
      void loadWaiting()
      void loadAgentWaits()
    }, debounceMs)
  }

  /** The same pause for what only the idle agents' words depend on (a wait, a task, a report): no call of `ListWaiting`. */
  const scheduleWaits = () => {
    if (stopped || pause !== undefined || waitsPause !== undefined) return
    waitsPause = timers.setTimeout(() => {
      waitsPause = undefined
      void loadAgentWaits()
    }, debounceMs)
  }

  async function loadPullRequests(): Promise<void> {
    const st = store.getState()
    if (prsLoading || st.conn.kind !== 'ready' || !st.synced) return
    // The host answers per run (a device names it): one call for each run still open, side by side.
    const runIds = Object.values(st.runs).filter((r) => r.status !== RunStatus.CLOSED).map((r) => r.id)
    prsLoading = true
    const seenPr = prTouches
    let again = false
    try {
      const results = await Promise.allSettled(runIds.map((id) => listPullRequests(client as MeshClient, id)))
      if (stopped) return
      if (seenPr !== prTouches) {
        // An event (a merge, a hold) came while the call was on its way: its answer may predate it. Keep what the event said, and ask again.
        again = true
        return
      }
      const agents = results.flatMap((r) => (r.status === 'fulfilled' ? r.value.agents : []))
      const failed = results.find((r): r is PromiseRejectedResult => r.status === 'rejected')
      // A run that failed keeps what was known of its agents; the others are replaced.
      const failedRuns = new Set(results.flatMap((r, i) => (r.status === 'rejected' ? [runIds[i]!] : [])))
      const kept = Object.values(store.getState().pullRequests).filter((p) => failedRuns.has(store.getState().agents[p.agentId]?.runId ?? ''))
      store.setState(setPullRequests([...agents, ...kept]))
      // The reason GitHub can't be read, when any run's answer gives one (a host with no `gh` says it for every run).
      store.setState(setGithubProblem(results.flatMap((r) => (r.status === 'fulfilled' && r.value.githubProblem ? [r.value.githubProblem] : []))[0] ?? ''))
      if (failed) {
        // Said once (a host without `gh` or an older daemon would say it every minute); the buckets then lack "ready to review".
        if (!prsComplained) log(`couldn't read the pull requests: ${failed.reason instanceof Error ? failed.reason.message : String(failed.reason)}`)
        prsComplained = true
      } else prsComplained = false
    } finally {
      prsLoading = false
      if (again) void loadPullRequests()
    }
  }

  async function loadAgents(): Promise<void> {
    const s = store.getState()
    if (s.conn.kind !== 'ready' || !s.synced) return
    if (waitingFailed) {
      // One call per tick, so no pause is needed.
      waitingFailed = false
      waitingTouches++
      void loadWaiting()
    }
    const seen = agentTouches
    try {
      const res = await client.listAgents({})
      if (stopped || seen !== agentTouches) return
      const next: Record<string, Agent> = {}
      let changed = false
      for (const a of res.agents) {
        const had = store.getState().agents[a.id]
        // An agent that didn't change keeps its object, so nothing that shows it redraws.
        next[a.id] = had && equals(AgentSchema, had, a) ? had : a
        if (next[a.id] !== had) changed = true
      }
      if (changed) {
        store.setState((st) => ({ agents: { ...st.agents, ...next } }))
        // A prompt aged into unknown, or the other way round, changes what waits.
        waitingTouches++
        schedule()
      }
    } catch (e) {
      complain('refresh the agents', e)
    }
  }

  return {
    /** Call with every event of the stream, after it was applied to the store. */
    event(e: Event): void {
      const p = e.payload
      switch (p.case) {
        case 'messageSent': {
          const t = p.value.message?.type
          if (t === MessageType.PLAN || t === MessageType.QUESTION || t === MessageType.ANSWER) {
            waitingTouches++
            schedule()
          } else if (t === MessageType.WAIT || t === MessageType.TASK || t === MessageType.DONE || t === MessageType.FAILED) {
            // These change what an idle agent waits on (`ListAgentWaits`), not what waits on the user.
            scheduleWaits()
          }
          return
        }
        case 'agentSpawned':
        case 'agentStateChanged':
        case 'agentExited':
        case 'agentRetired':
          agentTouches++
          waitingTouches++
          schedule()
          return
        // These change an agent the store holds too, so a list of the agents that was on its way is older than them.
        case 'agentArchived':
        case 'agentReopenRequested':
        case 'agentReopenDropped':
        case 'agentReopened':
        case 'agentPhaseChanged':
        case 'agentRoleChanged':
        case 'agentAdopted':
          agentTouches++
          return
        case 'pullRequestChanged':
          prTouches++
          return
        case 'permissionChanged':
          void loadPermissions()
          return
        case 'runStopped':
          waitingTouches++
          schedule()
          return
      }
    },
    /** A snapshot arrived (the first connection, or a resync): list what waits at once. */
    synced(): void {
      waitingTouches++
      timers.clearTimeout(pause)
      pause = undefined
      timers.clearTimeout(waitsPause)
      waitsPause = undefined
      void loadWaiting()
      void loadAgentWaits()
      void loadPullRequests()
      void loadPermissions()
    },
    start(): void {
      ticker ??= timers.setInterval(() => void loadAgents(), agentsEveryMs)
      prTicker ??= timers.setInterval(() => void loadPullRequests(), prsEveryMs)
      waitsTicker ??= timers.setInterval(() => void loadAgentWaits(), waitsEveryMs)
    },
    stop(): void {
      stopped = true
      timers.clearTimeout(pause)
      pause = undefined
      timers.clearTimeout(waitsPause)
      waitsPause = undefined
      timers.clearInterval(ticker)
      ticker = undefined
      timers.clearInterval(prTicker)
      prTicker = undefined
      timers.clearInterval(waitsTicker)
      waitsTicker = undefined
    },
  }
}
