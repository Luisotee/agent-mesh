import { listMessages } from './actions'
import type { MeshClient } from './actions'
import { RunStatus, type Message } from './gen/agentmesh/v1/types_pb'
import { mergeMessages, type HostStore } from './store'

export const timeOf = (m: Message): number => Number(m.createdAt?.seconds ?? 0n) * 1000 + Math.floor((m.createdAt?.nanos ?? 0) / 1e6)

/** Oldest first, as the host orders them (time, then id). */
export const byTime = (a: Message, b: Message): number => timeOf(a) - timeOf(b) || a.id.localeCompare(b.id)

/**
 * Loads the stored messages of the open runs of a host into its store, a page at a time: the event stream starts
 * from a snapshot, so what was said before the app connected comes only from `ListMessages`. A run that loaded
 * completely once resumes after its newest message on a later snapshot; one that didn't starts over.
 */
export function messageLoader(client: Pick<MeshClient, 'listMessages'>, store: HostStore, log: (line: string) => void) {
  const complete = new Set<string>()
  const running = new Set<string>()

  async function loadRun(runId: string): Promise<void> {
    if (running.has(runId)) return
    running.add(runId)
    try {
      let after = ''
      if (complete.has(runId)) {
        const mine = Object.values(store.getState().messages).filter((m) => m.runId === runId).sort(byTime)
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
    } catch (e) {
      log(`couldn't load the messages of run ${runId}: ${e instanceof Error ? e.message : String(e)}`)
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
