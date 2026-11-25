import { Plus } from 'lucide-react'
import { useState, type ReactElement } from 'react'
import { spawnAgent, startRun } from '@/actions'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogTrigger } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Input, Select, Textarea } from '@/components/ui/input'
import { useApp, useHostState } from '@/context'
import { knownRepos } from '@/derive'
import { AgentRole } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/** StartRun: a run is a title; its agents come after. */
export function NewRunDialog({ host }: { host: HostRuntime }) {
  const app = useApp()
  const [open, setOpen] = useState(false)
  const [title, setTitle] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const { run } = await startRun(host.client, title.trim())
      setOpen(false)
      setTitle('')
      if (run) app.selectHost(host.info.id)
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="secondary" />}>
        <Plus /> New run
      </DialogTrigger>
      <DialogContent title="New run" description="A run groups a lead agent and its workers across repos.">
        <form onSubmit={submit} className="flex flex-col gap-3">
          <Field label="Title">
            <Input autoFocus value={title} onChange={(e) => setTitle(e.target.value)} placeholder="What the run is for" />
          </Field>
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

/** SpawnAgent: start an agent in a new worktree of a repo on the host. */
export function SpawnDialog({ host, runId, trigger }: { host: HostRuntime; runId: string; trigger: ReactElement }) {
  const agents = useHostState(host, (s) => s.agents)
  const hasLead = Object.values(agents).some((a) => a.runId === runId && a.role === AgentRole.ROOT)
  const [open, setOpen] = useState(false)
  const [repo, setRepo] = useState('')
  const [slug, setSlug] = useState('')
  const [role, setRole] = useState<AgentRole>(hasLead ? AgentRole.WORKER : AgentRole.ROOT)
  const [kind, setKind] = useState('claude')
  const [model, setModel] = useState('')
  const [task, setTask] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const repos = knownRepos(agents)
  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await spawnAgent(host.client, { runId, repo: repo.trim(), slug: slug.trim(), role, kind: kind.trim(), model: model.trim(), effort: '', task: task.trim() })
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
    <Dialog open={open} onOpenChange={setOpen}>
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
              <Input required value={kind} onChange={(e) => setKind(e.target.value)} />
            </Field>
            <Field label="Model" hint="Empty: the CLI's default.">
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
