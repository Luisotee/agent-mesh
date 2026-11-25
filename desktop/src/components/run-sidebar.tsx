import { Plus, UserPlus } from 'lucide-react'
import { useMemo } from 'react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { useApp, useAppState, useHostState } from '@/context'
import { buildTrees, repoName, ROLE_LABEL, statusSentence, type TreeNode } from '@/derive'
import { RunStatus } from '@/gen/agentmesh/v1/types_pb'
import { cn } from '@/lib/utils'
import type { HostRuntime } from '@/model'
import { StateIcon } from './state-icon'
import { WaitingList } from './waiting-list'
import { NewRunDialog, SpawnDialog } from './spawn-dialogs'

/** The host's runs, each as a tree from its lead: workers under it, their read-only agents under them. */
export function RunSidebar({ host }: { host: HostRuntime }) {
  const runs = useHostState(host, (s) => s.runs)
  const agents = useHostState(host, (s) => s.agents)
  const trees = useMemo(() => buildTrees(runs, agents), [runs, agents])
  return (
    <nav aria-label="Runs" className="flex min-h-0 flex-1 flex-col">
      <WaitingList host={host} />
      <div className="flex items-center justify-between px-3 py-2">
        <h2 className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">Runs</h2>
        <NewRunDialog host={host} />
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-3">
        {trees.length === 0 && (
          <p className="px-2 py-6 text-center text-xs text-muted-foreground">No runs yet. Start one to give a lead agent its work.</p>
        )}
        {trees.map((t) => (
          <section key={t.run.id} className="mb-3" aria-label={t.run.title || t.run.id}>
            <header className="flex items-center gap-2 px-2 py-1">
              <h3 className="min-w-0 flex-1 truncate text-[13px] font-semibold">{t.run.title || 'Untitled run'}</h3>
              {t.run.status === RunStatus.CLOSED && <Badge>closed</Badge>}
              {t.needsInput > 0 && <Badge className="border-state-needs text-state-needs">{t.needsInput} need you</Badge>}
              <SpawnDialog host={host} runId={t.run.id} trigger={<Button size="icon" variant="ghost" className="size-6" aria-label={`Add an agent to ${t.run.title || t.run.id}`}><UserPlus className="size-3.5" /></Button>} />
            </header>
            <p className="px-2 pb-1 font-mono text-[11px] text-muted-foreground">{t.run.id}</p>
            {t.roots.length === 0 && <p className="px-2 py-1 text-xs text-muted-foreground">No agents. <Plus className="inline size-3" /> adds one.</p>}
            <ul>
              {t.roots.map((n) => (
                <TreeRow key={n.agent.id} node={n} host={host} depth={0} />
              ))}
            </ul>
          </section>
        ))}
      </div>
    </nav>
  )
}

function TreeRow({ node, host, depth }: { node: TreeNode; host: HostRuntime; depth: number }) {
  const app = useApp()
  const selected = useAppState((s) => s.hostId === host.info.id && s.agentId === node.agent.id)
  const a = node.agent
  return (
    <li>
      <button
        type="button"
        aria-current={selected ? 'true' : undefined}
        onClick={() => app.selectAgent(host.info.id, a.id)}
        className={cn('flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-accent', selected && 'bg-accent')}
      >
        <span className="mt-0.5"><StateIcon agent={a} /></span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1.5">
            <span className="truncate font-medium">{a.name}</span>
            <Badge>{ROLE_LABEL[a.role]}</Badge>
          </span>
          <span className="block truncate text-xs text-muted-foreground">{statusSentence(a)}</span>
          <span className="block truncate text-[11px] text-muted-foreground">
            {repoName(a.repo)} · {a.kind}{a.model ? ` · ${a.model}` : ''}
          </span>
        </span>
      </button>
      {node.children.length > 0 && (
        <ul className={cn('ml-4 border-l pl-1', depth > 2 && 'ml-2')}>
          {node.children.map((c) => (
            <TreeRow key={c.agent.id} node={c} host={host} depth={depth + 1} />
          ))}
        </ul>
      )}
    </li>
  )
}
