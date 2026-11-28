import { useState } from 'react'
import { listProjects } from '@/actions'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { useApp, useAppState } from '@/context'
import type { Project } from '@/gen/agentmesh/v1/mesh_pb'
import type { HostRuntime } from '@/model'
import { usePolled } from '@/polled'
import { rankProjects } from '@/projects'

/** Rows the shape of the project list's (a name over a path), while the host's projects are on their way. */
export function ProjectRowsSkeleton({ label, rows = 4 }: { label: string; rows?: number }) {
  return (
    <div role="status" aria-label={label} className="flex flex-col">
      <span className="sr-only">{label}</span>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex flex-col gap-1.5 px-2 py-1.5">
          <Skeleton className="h-3.5 w-1/3" />
          <Skeleton className={i % 2 ? 'w-1/2' : 'w-2/3'} />
        </div>
      ))}
    </div>
  )
}

const reason = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/**
 * Adds a project to the sidebar with no run in it yet: it is pinned there (device-local) until unpinned. A name picks one
 * the host knows; a path is checked with the host first (`ListProjects` of that repo, `quick`: no fetch and no pull requests, so
 * Enter answers at once), so what is not a git repository is refused with the host's reason instead of being pinned as an empty row.
 */
export function AddProjectDialog({ host, onClose }: { host: HostRuntime; onClose: () => void }) {
  const app = useApp()
  const [query, setQuery] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const pinned = useAppState((s) => s.pins[host.info.id]) ?? []
  const list = usePolled(async () => (await listProjects(host.client)).projects, `projects/${host.info.id}`, 60_000)
  const shown = rankProjects(list.value ?? [], query, pinned).filter((p) => !pinned.includes(p.path)).slice(0, 8)
  const add = (p: Pick<Project, 'path'>) => {
    app.togglePin(host.info.id, p.path, true)
    onClose()
  }
  /** Enter: the path typed when it is one (a project of the list by that exact path, or a new one the host must accept), else the best match. */
  async function submit() {
    if (busy) return
    const typed = query.trim()
    const exact = (list.value ?? []).find((p) => p.path === typed)
    if (exact) return add(exact)
    // An absolute path only: the host does not expand a `~` it is sent, and a relative one means its own working directory.
    if (typed.startsWith('/')) {
      setBusy(true)
      setError('')
      try {
        const res = await listProjects(host.client, typed, true)
        const found = res.projects[0]
        if (!found) throw new Error(`${typed} is not a git repository on ${host.info.name}`)
        add(found)
      } catch (e) {
        setError(`${typed}: ${reason(e)}`)
      } finally {
        setBusy(false)
      }
      return
    }
    if (shown[0]) return add(shown[0])
    // A name already pinned matches nothing new: say so rather than "no match".
    const pinnedMatch = rankProjects(list.value ?? [], query, pinned)[0]
    if (pinnedMatch) return setError(`${pinnedMatch.name} is in the sidebar already`)
    setError(typed ? `No project of ${host.info.name} matches "${typed}": type its path, starting with /` : 'Type a name or a path')
  }
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title="Add a project" description={`A git repository on ${host.info.name} to keep in the sidebar, pinned, before any run works in it.`}>
        <form className="flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); void submit() }}>
          <Field label="Project">
            <Input autoFocus value={query} onChange={(e) => { setQuery(e.target.value); setError('') }} placeholder="A name, or a path on the host" />
          </Field>
          {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
          {list.error && !list.value && <p role="alert" className="text-xs text-destructive">Couldn't list the host's projects: {list.error}</p>}
          {list.loading && !list.value && !list.error && <ProjectRowsSkeleton label="Loading the host's projects" />}
          <ul aria-label="Projects" className="flex flex-col">
            {shown.map((p) => (
              <li key={p.path}>
                <button type="button" onClick={() => add(p)} className="flex w-full flex-col rounded-md px-2 py-1.5 text-left hover:bg-accent">
                  <span className="text-[13px] font-medium">{p.name}</span>
                  <span className="truncate font-mono text-xs text-muted-foreground">{p.path}</span>
                </button>
              </li>
            ))}
          </ul>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={busy || !query.trim()}>{busy ? 'Checking…' : 'Add'}</Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  )
}
