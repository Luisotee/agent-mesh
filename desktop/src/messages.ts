import { listMessages } from './actions'
import type { MeshClient } from './actions'
import { compareMessages } from './order'
import { mergeMessages, setMessageLoad, type HostStore } from './store'

const MAX_RETRIES = 5
const RETRY_BASE_MS = 1000

/**
 * Loads the stored messages of one run at a time, a page at a time, when the run's Messages tab is first shown: the
 * event stream starts from a snapshot, so what was said before the app connected comes only from `ListMessages`,
 * and nothing else in the app needs it (what waits on the user is `ListWaiting`). A run that loaded completely once
 * is topped up after its newest message when a later snapshot arrives; one that didn't starts over.
 */
export function messageLoader(client: Pick<MeshClient, 'listMessages'>, store: HostStore, log: (line: string) => void) {
  /** Runs that loaded completely once. */
  const complete = new Set<string>()
  /** Runs somebody asked for: they are topped up on a snapshot. */
  const wanted = new Set<string>()
  const running = new Map<string, Promise<void>>()
  const retries = new Map<string, number>()
  const timers = new Map<string, ReturnType<typeof setTimeout>>()

  function loadRun(runId: string): Promise<void> {
    const going = running.get(runId)
    if (going) return going
    // A retry still waiting is replaced by this load, not added to.
    clearTimeout(timers.get(runId))
    timers.delete(runId)
    const p = (async () => {
      // A run already shown stays `loaded` while it is topped up.
      if (!complete.has(runId)) store.setState(setMessageLoad(runId, 'loading'))
      try {
        let after = ''
        if (complete.has(runId)) {
          const mine = Object.values(store.getState().messages).filter((m) => m.runId === runId).sort(compareMessages)
          after = mine[mine.length - 1]?.id ?? ''
        }
        for (;;) {
          const page = await listMessages(client as MeshClient, runId, after)
          store.setState((s) => mergeMessages(s, page.messages))
          const last = page.messages[page.messages.length - 1]
          if (!page.more || !last) break
          after = last.id
        }
        // A run the host dropped (or the loader was stopped) while this ran is not recorded as loaded.
        if (!wanted.has(runId)) return
        complete.add(runId)
        retries.delete(runId)
        store.setState(setMessageLoad(runId, 'loaded'))
      } catch (e) {
        log(`couldn't load the messages of run ${runId}: ${e instanceof Error ? e.message : String(e)}`)
        if (!wanted.has(runId)) return
        if (!complete.has(runId)) store.setState(setMessageLoad(runId, 'failed'))
        // The Messages tab reads the history: try again, a few times, with a growing wait.
        const n = retries.get(runId) ?? 0
        if (n < MAX_RETRIES) {
          retries.set(runId, n + 1)
          timers.set(runId, setTimeout(() => void loadRun(runId), RETRY_BASE_MS * 2 ** n))
        }
      } finally {
        running.delete(runId)
      }
    })()
    running.set(runId, p)
    return p
  }

  return {
    /** Load a run's messages once (a run already loaded, or on its way, is left as it is). */
    ensure(runId: string): Promise<void> {
      wanted.add(runId)
      return complete.has(runId) ? Promise.resolve() : loadRun(runId)
    },
    /** Try again now (the Retry button), starting the back-off over. */
    retry(runId: string): Promise<void> {
      wanted.add(runId)
      retries.delete(runId)
      return loadRun(runId)
    },
    /** A snapshot arrived: top up what was asked for, and forget a run the host no longer lists. */
    resync(runs: Array<{ id: string }>): Promise<void[]> {
      const listed = new Set(runs.map((r) => r.id))
      for (const id of [...wanted]) {
        if (listed.has(id)) continue
        wanted.delete(id)
        complete.delete(id)
        retries.delete(id)
        clearTimeout(timers.get(id))
        timers.delete(id)
      }
      return Promise.all([...wanted].map((id) => loadRun(id)))
    },
    /** Stops the retries (the host was removed). */
    stop(): void {
      for (const t of timers.values()) clearTimeout(t)
      timers.clear()
      wanted.clear()
    },
  }
}
