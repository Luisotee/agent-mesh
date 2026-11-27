import { Pin, PinOff } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { listProjects, spawnAgent, startRun } from '@/actions'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogTrigger } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Input, Select, Textarea } from '@/components/ui/input'
import { useApp, useAppState, useHostState } from '@/context'
import { knownRepos } from '@/derive'
import { ProjectSchema, type Project } from '@/gen/agentmesh/v1/mesh_pb'
import { create } from '@bufbuild/protobuf'
import { AgentRole, RunStatus } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { rankProjects } from '@/projects'
import { cn } from '@/lib/utils'
import { RunAutonomyFields, useRunAutonomy } from './run-autonomy'
import { startablePullRequests, startPoint, type StartFrom } from '@/workspace-start'
import { vendorOfKind } from '@/vendor'
import { VendorIcon } from './vendor-icon'
import { tiersOf } from './spawn-dialogs'

const NO_PINS: string[] = []
const message = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/**
 * New workspace: a project on the host, where in it the work starts (its default branch, a branch, or a pull request's
 * branch), and the run it joins (a new one by default): an agent in a new git worktree of the project, in a terminal the
 * user can open. The first agent of a run is its lead, every later one a worker.
 */
export function NewWorkspaceDialog({ host, trigger }: { host: HostRuntime; trigger?: React.ReactElement }) {
  const app = useApp()
  const open = useAppState((s) => s.workspaceOpen)
  return (
    <Dialog open={open} onOpenChange={(o) => app.openWorkspace(o)}>
      {trigger && <DialogTrigger render={trigger} />}
      <DialogContent title="New workspace" description="Starts an agent in a new git worktree of a project on this host, on a branch of its own.">
        <WorkspaceForm host={host} onDone={() => app.openWorkspace(false)} />
      </DialogContent>
    </Dialog>
  )
}

function WorkspaceForm({ host, onDone }: { host: HostRuntime; onDone: () => void }) {
  const app = useApp()
  const agents = useHostState(host, (s) => s.agents)
  const runs = useHostState(host, (s) => s.runs)
  const settings = useHostState(host, (s) => s.settings)
  const [projects, setProjects] = useState<Project[]>([])
  const [listError, setListError] = useState('')
  // A project's "New lead here" opens the form on that project.
  const initialRepo = useAppState((s) => s.workspaceRepo)
  const [repo, setRepo] = useState(initialRepo ?? '')
  // What reading one project (fetch, branches, PRs) gave, for the path it was asked for.
  const [detail, setDetail] = useState<{ path: string; project?: Project; error?: string; loading: boolean }>()
  const [from, setFrom] = useState<StartFrom>({ kind: 'default' })
  const initialRun = useAppState((s) => s.workspaceRun)
  const [runChoice, setRunChoice] = useState(() => (initialRun && runs[initialRun] && runs[initialRun].status !== RunStatus.CLOSED ? initialRun : 'new'))
  const [title, setTitle] = useState('')
  const [slug, setSlug] = useState('')
  const [kind, setKind] = useState('claude')
  const [chosen, setChosen] = useState<string | undefined>()
  const [task, setTask] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const autonomy = useRunAutonomy()
  const pinned = useAppState((s) => s.pins[host.info.id]) ?? NO_PINS
  const read = useRef(0)
  // A run this form started that its agent then failed to join: a retry joins it, not a second empty run.
  const started = useRef<string | undefined>(undefined)

  useEffect(() => {
    let alive = true
    listProjects(host.client).then(
      (r) => alive && setProjects(r.projects),
      (e: unknown) => alive && setListError(message(e)),
    )
    return () => {
      alive = false
    }
  }, [host])

  const paths = useMemo(() => {
    const seen = new Set(projects.map((p) => p.path))
    // A host that lists none (an older daemon) still offers the repos agents used.
    return [...projects.map((p) => ({ path: p.path, name: p.name })), ...knownRepos(agents).filter((r) => !seen.has(r)).map((r) => ({ path: r, name: r.split('/').pop() ?? r }))]
  }, [projects, agents])
  // The list under the field, narrowed by what is typed (a chosen path narrows it to itself and its likes) and with the pinned first.
  const listed = useMemo(() => {
    const all = paths.map((p) => projects.find((x) => x.path === p.path) ?? create(ProjectSchema, { path: p.path, name: p.name }))
    return rankProjects(all, repo, pinned)
  }, [paths, projects, repo, pinned])

  /** Reads the project once the path is complete: it is one of the listed ones, or the user left the field. */
  function load(path: string) {
    const p = path.trim()
    // Read once per path; a read that failed is tried again (a fetch or gh that timed out).
    if (!p || (detail?.path === p && !detail.error)) return
    const mine = ++read.current
    setDetail({ path: p, loading: true })
    setFrom({ kind: 'default' })
    listProjects(host.client, p).then(
      (r) => mine === read.current && setDetail({ path: p, project: r.projects[0], loading: false }),
      (e: unknown) => mine === read.current && setDetail({ path: p, error: message(e), loading: false }),
    )
  }

  // The project the form opened on is read at once, as if it had been picked.
  const opened = useRef(false)
  useEffect(() => {
    if (opened.current || !initialRepo) return
    opened.current = true
    load(initialRepo)
  })

  const project = detail?.path === repo.trim() ? detail.project : undefined
  // A choice made for another project (the path was edited and the form sent without leaving the field) is no choice.
  const effective: StartFrom = project ? from : { kind: 'default' }
  const open = Object.values(runs).filter((r) => r.status !== RunStatus.CLOSED)
  const target = runChoice === 'new' ? undefined : runs[runChoice]
  const hasLead = !!target && Object.values(agents).some((a) => a.runId === target.id && a.role === AgentRole.ROOT)
  const role = hasLead ? AgentRole.WORKER : AgentRole.ROOT
  const tiers = useMemo(() => tiersOf(settings, kind.trim()), [settings, kind])
  // A worker always runs on a tier (`mid` when the host has one); a lead may take the CLI's default.
  const tier = role === AgentRole.ROOT ? (chosen ?? '') : chosen && tiers.includes(chosen) ? chosen : tiers.includes('mid') ? 'mid' : (tiers[0] ?? '')

  // A worker runs on a tier: with none listed for the CLI there is nothing to start it on.
  const noTier = role === AgentRole.WORKER && !tier

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (busy || noTier) return
    setBusy(true)
    setError('')
    try {
      let runId = target?.id ?? started.current
      if (!runId) {
        const res = await startRun(host.client, title.trim() || slug.trim(), autonomy.value())
        runId = res.run?.id
        if (!runId) throw new Error('the host started no run')
        started.current = runId
      }
      const res = await spawnAgent(host.client, { runId, repo: repo.trim(), slug: slug.trim(), role, kind: kind.trim(), tier, model: '', effort: '', task: task.trim(), base: startPoint(effective) })
      started.current = undefined
      onDone()
      if (res.agent) app.selectAgent(host.info.id, res.agent.id, 'terminal')
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-3">
      <Field label="Project" hint={listError ? `The host's project list isn't available (${listError}): type a path.` : 'A git repository on the host.'}>
        <Input
          autoFocus
          required
          value={repo}
          onChange={(e) => {
            setRepo(e.target.value)
            setFrom({ kind: 'default' })
            if (paths.some((p) => p.path === e.target.value)) load(e.target.value)
          }}
          onBlur={() => load(repo)}
          placeholder="/home/you/projects/app"
        />
      </Field>
      {listed.length > 0 && (
        <ul aria-label="Repos on this host" className="-mt-1 max-h-40 overflow-y-auto rounded-md border">
          {listed.map((p) => {
            const isPinned = pinned.includes(p.path)
            return (
              <li key={p.path} className={cn('flex items-center gap-1 pr-1 hover:bg-accent', repo.trim() === p.path && 'bg-accent')}>
                <button
                  type="button"
                  className="min-w-0 flex-1 px-2.5 py-1 text-left text-xs"
                  onClick={() => {
                    setRepo(p.path)
                    setFrom({ kind: 'default' })
                    load(p.path)
                  }}
                >
                  <span className="font-medium">{p.name}</span> <span className="text-muted-foreground">{p.path}</span>
                </button>
                <Button
                  type="button"
                  size="icon"
                  variant="ghost"
                  className="size-6 shrink-0"
                  aria-pressed={isPinned}
                  aria-label={`${isPinned ? 'Unpin' : 'Pin'} ${p.name}`}
                  title={isPinned ? 'Unpin' : 'Pin to the top'}
                  onClick={() => app.togglePin(host.info.id, p.path)}
                >
                  {isPinned ? <PinOff className="size-3.5" /> : <Pin className="size-3.5" />}
                </Button>
              </li>
            )
          })}
        </ul>
      )}

      <Field label="Start from" hint={detail?.loading ? 'Reading the project…' : detail?.error ? detail.error : project ? `Default branch: ${project.defaultBranch || 'the repo\'s HEAD'}` : 'Pick the project first.'}>
        <div className="grid grid-cols-2 gap-2">
          <Select
            aria-label="Start from"
            value={from.kind}
            disabled={!project}
            onChange={(e) => setFrom({ kind: e.target.value as StartFrom['kind'], ref: '' } as StartFrom)}
          >
            <option value="default">Its default branch</option>
            <option value="branch">A branch</option>
            <option value="pr">A pull request</option>
          </Select>
          {from.kind === 'branch' && (
            <Select aria-label="Branch" value={from.ref ?? ''} onChange={(e) => setFrom({ kind: 'branch', ref: e.target.value })}>
              <option value="">Choose a branch…</option>
              {project?.branches.map((b) => <option key={b} value={b}>{b}</option>)}
            </Select>
          )}
          {from.kind === 'pr' && (
            <Select aria-label="Pull request" value={from.ref ?? ''} onChange={(e) => setFrom({ kind: 'pr', ref: e.target.value })}>
              <option value="">{project?.pullRequests.some((p) => !p.crossRepository) ? 'Choose a pull request…' : 'No open pull requests'}</option>
              {startablePullRequests(project?.pullRequests ?? []).map((p) => (
                <option key={p.number} value={p.crossRepository ? '' : p.headRef} disabled={p.crossRepository}>
                  #{p.number} {p.title}{p.draft ? ' (draft)' : ''}{p.crossRepository ? ' (from a fork: not available)' : ''}
                </option>
              ))}
            </Select>
          )}
        </div>
      </Field>

      <div className="grid grid-cols-2 gap-3">
        <Field label="Run" hint={runChoice === 'new' ? 'Starts a run, with this as its lead.' : hasLead ? 'Joins the run as a worker.' : 'Joins the run as its lead.'}>
          <Select aria-label="Run" value={runChoice} onChange={(e) => setRunChoice(e.target.value)}>
            <option value="new">A new run</option>
            {open.map((r) => <option key={r.id} value={r.id}>{r.title || r.id}</option>)}
          </Select>
        </Field>
        {runChoice === 'new' && (
          <Field label="Run title" hint="Optional.">
            <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="What it is for" />
          </Field>
        )}
        {runChoice === 'new' && (
          <div className="col-span-2 flex flex-col gap-3">
            <RunAutonomyFields host={host} state={autonomy} />
          </div>
        )}
        <Field label="Name" hint="Names the branch and the agent.">
          <Input required value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="fix-typo" pattern="[a-z0-9][a-z0-9-]*" />
        </Field>
        <Field label="CLI">
          <div className="flex items-center gap-2">
            <VendorIcon vendor={vendorOfKind(kind)} kind={kind} />
            <Input required value={kind} onChange={(e) => setKind(e.target.value)} />
          </div>
        </Field>
        <Field label="Tier" hint="Each tier is a model and effort in the host's settings.">
          <Select aria-label="Tier" value={tier} disabled={noTier} onChange={(e) => setChosen(e.target.value)}>
            {role === AgentRole.ROOT && <option value="">The CLI's default</option>}
            {tiers.length === 0 && role === AgentRole.WORKER && <option value="">The host lists no tier for this CLI</option>}
            {tiers.map((t) => <option key={t} value={t}>{t}</option>)}
          </Select>
        </Field>
      </div>
      <Field label="First message" hint="Sent as a task once the agent is ready; empty starts it idle.">
        <Textarea value={task} onChange={(e) => setTask(e.target.value)} />
      </Field>
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <Button type="submit" disabled={busy || noTier || (effective.kind !== 'default' && !effective.ref)}>Start workspace</Button>
    </form>
  )
}
