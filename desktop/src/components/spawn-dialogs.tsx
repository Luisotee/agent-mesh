import { Plus } from 'lucide-react'
import { useMemo, useState, type ReactElement } from 'react'
import { spawnAgent, startRun } from '@/actions'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogTrigger } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Input, Select, Textarea } from '@/components/ui/input'
import { useHostState } from '@/context'
import { knownRepos } from '@/derive'
import { AgentRole } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { vendorOfKind } from '@/vendor'
import { RunAutonomyFields, useRunAutonomy } from './run-autonomy'
import { VendorIcon } from './vendor-icon'

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/** StartRun: a run is a title; its agents come after. */
export function NewRunDialog({ host, label = 'New run', variant = 'secondary' }: { host: HostRuntime; label?: string; variant?: 'secondary' | 'outline' }) {
  const [open, setOpen] = useState(false)
  const [title, setTitle] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const autonomy = useRunAutonomy()
  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await startRun(host.client, title.trim(), autonomy.value())
      setOpen(false)
      setTitle('')
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant={variant} />}>
        <Plus /> {label}
      </DialogTrigger>
      <DialogContent title="New run" description="A run groups a lead agent and its workers across repos.">
        <form onSubmit={submit} className="flex flex-col gap-3">
          <Field label="Title">
            <Input autoFocus value={title} onChange={(e) => setTitle(e.target.value)} placeholder="What the run is for" />
          </Field>
          <RunAutonomyFields host={host} state={autonomy} />
          {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
          <Button type="submit" disabled={busy}>Start run</Button>
        </form>
      </DialogContent>
    </Dialog>
  )
}

const ROLES: Array<[AgentRole, string]> = [
  [AgentRole.ROOT, 'Lead (root)'],
  [AgentRole.WORKER, 'Worker'],
  [AgentRole.ASKER, 'Asker (read-only)'],
  [AgentRole.REVIEWER, 'Reviewer (read-only)'],
]

const FALLBACK_TIERS = ['light', 'mid', 'strong', 'top']

/** The tiers a host defines for a CLI (`tiers.<cli>.<tier>.*` settings), or the usual four before they are loaded. */
export function tiersOf(settings: Record<string, unknown>, kind: string): string[] {
  const found = new Set<string>()
  for (const key of Object.keys(settings)) {
    const m = /^tiers\.([^.]+)\.([^.]+)\./.exec(key)
    if (m && m[1] === kind) found.add(m[2]!)
  }
  return found.size > 0 ? [...found] : FALLBACK_TIERS
}

/** SpawnAgent: start an agent in a new worktree of a repo on the host. */
export function SpawnDialog({ host, runId, trigger }: { host: HostRuntime; runId: string; trigger: ReactElement }) {
  const agents = useHostState(host, (s) => s.agents)
  const settings = useHostState(host, (s) => s.settings)
  const hasLead = Object.values(agents).some((a) => a.runId === runId && a.role === AgentRole.ROOT)
  const [open, setOpen] = useState(false)
  const [repo, setRepo] = useState('')
  const [slug, setSlug] = useState('')
  const defaultRole = hasLead ? AgentRole.WORKER : AgentRole.ROOT
  const [role, setRole] = useState<AgentRole>(defaultRole)
  const [kind, setKind] = useState('claude')
  // What the user chose; unset, the tier shown is the default for the role (see `tier` below).
  const [chosen, setChosen] = useState<string | undefined>()
  const [model, setModel] = useState('')
  const [task, setTask] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const repos = knownRepos(agents)
  const tiers = useMemo(() => tiersOf(settings, kind.trim()), [settings, kind])
  // Every agent but a lead runs on a tier or a model the user names: the host has no default model, so a worker
  // always has a tier (`mid` when the host has one, else its first). A lead, the user's own session, may take the
  // CLI's default (no tier).
  const tier = role === AgentRole.ROOT ? (chosen ?? '') : chosen && tiers.includes(chosen) ? chosen : tiers.includes('mid') ? 'mid' : tiers[0]!
  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await spawnAgent(host.client, { runId, repo: repo.trim(), slug: slug.trim(), role, kind: kind.trim(), tier, model: model.trim(), effort: '', task: task.trim() })
      setOpen(false)
      setSlug('')
      setTask('')
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        // A run's first agent is its lead, every later one a worker, whatever the last spawn was.
        if (o) {
          setRole(defaultRole)
          setChosen(undefined)
        }
        setOpen(o)
      }}
    >
      <DialogTrigger render={trigger} />
      <DialogContent title="Add an agent" description="Starts the CLI in a new git worktree of the repo, on a branch of this run.">
        <form onSubmit={submit} className="flex flex-col gap-3">
          <Field label="Repository" hint="An absolute path on the host.">
            <Input list={`repos-${runId}`} autoFocus required value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="/home/you/projects/app" />
          </Field>
          <datalist id={`repos-${runId}`}>{repos.map((r) => <option key={r} value={r} />)}</datalist>
          <div className="grid grid-cols-2 gap-3">
            <Field label="Slug" hint="Names the branch and the agent.">
              <Input required value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="fix-typo" pattern="[a-z0-9][a-z0-9-]*" />
            </Field>
            <Field label="Role">
              <Select value={role} onChange={(e) => setRole(Number(e.target.value))}>
                {ROLES.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
              </Select>
            </Field>
            <Field label="CLI">
              <div className="flex items-center gap-2">
                <VendorIcon vendor={vendorOfKind(kind)} kind={kind} />
                <Input required value={kind} onChange={(e) => setKind(e.target.value)} />
              </div>
            </Field>
            <Field label="Tier" hint="What the host runs it on: each tier is a model and effort in the host's settings.">
              <Select value={tier} onChange={(e) => setChosen(e.target.value)}>
                {role === AgentRole.ROOT && <option value="">The CLI's default</option>}
                {tiers.map((t) => <option key={t} value={t}>{t}</option>)}
              </Select>
            </Field>
            <Field label="Model" hint="Only if you want one named; it replaces the tier's model.">
              <Input value={model} onChange={(e) => setModel(e.target.value)} />
            </Field>
          </div>
          <Field label="First message" hint="Sent as a task once the agent is ready.">
            <Textarea value={task} onChange={(e) => setTask(e.target.value)} />
          </Field>
          {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
          <Button type="submit" disabled={busy}>Start agent</Button>
        </form>
      </DialogContent>
    </Dialog>
  )
}
