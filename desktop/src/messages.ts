import { listMessages } from './actions'
import type { MeshClient } from './actions'
import { RunStatus } from './gen/agentmesh/v1/types_pb'
import { compareMessages } from './order'
import { mergeMessages, type HostStore } from './store'

const MAX_RETRIES = 5
const RETRY_BASE_MS = 1000

/**
 * Loads the stored messages of the open runs of a host into its store, a page at a time: the event stream starts
 * from a snapshot, so what was said before the app connected comes only from `ListMessages`. A run that loaded
 * completely once resumes after its newest message on a later snapshot; one that didn't starts over.
 */
export function messageLoader(client: Pick<MeshClient, 'listMessages'>, store: HostStore, log: (line: string) => void) {
  const complete = new Set<string>()
  const running = new Set<string>()
  const retries = new Map<string, number>()

  async function loadRun(runId: string): Promise<void> {
    if (running.has(runId)) return
    running.add(runId)
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
      complete.add(runId)
      retries.delete(runId)
    } catch (e) {
      log(`couldn't load the messages of run ${runId}: ${e instanceof Error ? e.message : String(e)}`)
      // The Messages tab reads the history: try again, a few times, with a growing wait.
      const n = retries.get(runId) ?? 0
      if (n < MAX_RETRIES) {
        retries.set(runId, n + 1)
        setTimeout(() => void loadRun(runId), RETRY_BASE_MS * 2 ** n)
      }
    } finally {
      running.delete(runId)
    }
  }

  return {
    /** Load the open runs of a snapshot. */
    load(runs: Array<{ id: string; status: RunStatus }>): Promise<void[]> {
      return Promise.all(runs.filter((r) => r.status === RunStatus.OPEN).map((r) => loadRun(r.id)))
    },
  }
}
