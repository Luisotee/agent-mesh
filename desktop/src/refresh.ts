import { equals } from '@bufbuild/protobuf'
import type { MeshClient } from './actions'
import { listWaiting } from './actions'
import { realTimers, type Timers } from './connection'
import { AgentSchema, MessageType, type Agent, type Event } from './gen/agentmesh/v1/types_pb'
import { setWaiting, type HostStore } from './store'

/**
 * Two things the event stream doesn't carry, read from the host again when they may have changed.
 *
 * What waits on the user is the host's to say (`ListWaiting`), so it is listed again, after a short pause that
 * folds a burst of events into one call, whenever an event could change it: a plan, question or answer, an agent
 * starting, changing state, exiting or being retired, a run stopping. A list that was on its way while one of those
 * arrived may be stale: it is shown, and fetched again after the pause.
 *
 * An agent that went quiet shows as `unknown` only when the host is asked (the state is derived from the time and
 * the terminal's output when read, never stored or sent as an event), so the agents are listed again every
 * `agentsEveryMs`; an agent an event touched meanwhile keeps what the event said.
 */
export function hostRefresher(
  client: Pick<MeshClient, 'listWaiting' | 'listAgents'>,
  store: HostStore,
  log: (line: string) => void,
  timers: Timers = realTimers,
  opts: { debounceMs?: number; agentsEveryMs?: number } = {},
) {
  const debounceMs = opts.debounceMs ?? 200
  const agentsEveryMs = opts.agentsEveryMs ?? 30_000
  let stopped = false
  let waitingTouches = 0
  let agentTouches = 0
  let pause: unknown
  let waitingLoading = false
  let ticker: unknown
  let complained = false
  let waitingFailed = false

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
      store.setState(setWaiting(res.waiting))
      if (seen !== waitingTouches) schedule()
    } catch (e) {
      // The list stays as it was; the next tick asks again.
      waitingFailed = true
      complain('list what waits on you', e)
    } finally {
      waitingLoading = false
    }
  }

  /** At most one call per `debounceMs`, however many events come: a burst is one call, and a steady stream can't starve it. */
  const schedule = () => {
    if (stopped || pause !== undefined) return
    pause = timers.setTimeout(() => {
      pause = undefined
      void loadWaiting()
    }, debounceMs)
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
        case 'agentReopenRequested':
        case 'agentReopenDropped':
        case 'agentReopened':
        case 'agentPhaseChanged':
        case 'agentRoleChanged':
        case 'agentAdopted':
          agentTouches++
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
      void loadWaiting()
    },
    start(): void {
      ticker ??= timers.setInterval(() => void loadAgents(), agentsEveryMs)
    },
    stop(): void {
      stopped = true
      timers.clearTimeout(pause)
      pause = undefined
      timers.clearInterval(ticker)
      ticker = undefined
    },
  }
}
