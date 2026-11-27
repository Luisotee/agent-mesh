import type { Shell, ShellChange } from './gen/agentmesh/v1/mesh_pb'
import { projectKey } from './sidebar'

/**
 * A project shell's tab is an ordinary tab (`workspace.ts`) whose "agent id" is `shell:<terminal id>`: the tabs, panes, drag,
 * layout and Ctrl+Tab all key on that string, so a shell needs no second kind of tab. The shells themselves are the host's
 * (`ListShells`), kept in the app state by host id.
 */
const PREFIX = 'shell:'

export const shellTabId = (shellId: string) => PREFIX + shellId
export const isShellTab = (agentId: string | undefined): agentId is string => !!agentId && agentId.startsWith(PREFIX)
export const shellIdOf = (agentId: string) => agentId.slice(PREFIX.length)

/** The project a shell tab is in, as the sidebar files projects (`sidebar.ts` `projectKey`): its host and folder. */
export const shellProject = (hostId: string, s: Shell) => projectKey(hostId, s.repo)

/** The shells of a host that no tab shows: what the + menu offers to open again. */
export function closedShells(shells: Shell[] | undefined, tabs: ReadonlyArray<{ hostId: string; agentId: string }>, hostId: string, repo?: string): Shell[] {
  return (shells ?? []).filter((s) => !s.exited && (!repo || s.repo === repo) && !tabs.some((t) => t.hostId === hostId && t.agentId === shellTabId(s.id)))
}

/**
 * What a `ShellChange` frame does to the app's shell lists: an opened shell joins its host's list; an ended one leaves it, or
 * stays marked exited while a tab still shows it (the tab keeps the shell's last output until the user closes it). A host
 * whose list was not loaded yet is left to that load, which reads the host's own list.
 */
export function applyShellChange<S extends { shells: Record<string, Shell[]>; tabs: ReadonlyArray<{ hostId: string; agentId: string }> }>(s: S, hostId: string, c: ShellChange): Partial<S> {
  const shell = c.shell
  const list = s.shells[hostId]
  if (!shell || !list) return {}
  let next: Shell[]
  if (!c.closed) next = [...list.filter((x) => x.id !== shell.id), shell]
  else if (s.tabs.some((t) => t.hostId === hostId && t.agentId === shellTabId(shell.id))) next = list.map((x) => (x.id === shell.id ? { ...x, exited: true } : x))
  else next = list.filter((x) => x.id !== shell.id)
  // An exited shell no tab shows any more has nothing left to say.
  next = next.filter((x) => !x.exited || s.tabs.some((t) => t.hostId === hostId && t.agentId === shellTabId(x.id)))
  return { shells: { ...s.shells, [hostId]: next } } as Partial<S>
}
