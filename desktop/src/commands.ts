import { approveAgent, retireAgent, ringAgent, startRun, stopAgent, switchToNextModel } from './actions'
import { canPair } from './components/pair-panel'
import { joinWaiting, ROLE_LABEL, statusSentence } from './derive'
import { AgentRole, AgentState, RunStatus, type Agent } from './gen/agentmesh/v1/types_pb'
import type { AgentTab, AppModel, HostRuntime } from './model'
import { DEFAULT_KEYS, appKeys, type KeyedCommand } from './prefs'
import { cycleTab as nextTab } from './workspace'

/**
 * Everything the palette offers and the shortcuts run. A command that acts on a host calls a function of
 * `actions.ts` (so it is a registry action a device may call: `registry.test.ts`, `commands.test.ts`); the rest only
 * change what the window shows.
 */
export interface Command {
  id: string
  title: string
  /** Where the palette lists it: Agents, Runs, Hosts, Waiting on you, Actions, View. */
  group: string
  /** More words the filter matches (the repo, the host, the status). */
  detail?: string
  /** Asks for one line of text first (the run's title). */
  ask?: { prompt: string; placeholder: string }
  /** Asks to be confirmed first: what Enter would do. */
  confirm?: string
  /** The shortcut setting that runs it (`app.keys.<key>`), if it can be bound. */
  key?: KeyedCommand
  run(input?: string): void | Promise<unknown>
}

const alive = (a: Agent) => a.state !== AgentState.EXITED && a.state !== AgentState.RETIRED

/** What waits on the user across every host, as places to go: a plan or question opens its messages, a prompt its terminal. */
export function waitingTargets(app: AppModel): Array<{ hostId: string; agentId: string; tab: AgentTab; text: string }> {
  return app.store.getState().hosts.flatMap((h) => {
    const s = h.store.getState()
    return joinWaiting(s.waiting, s.runs, s.agents).map((w) => ({
      hostId: h.info.id,
      agentId: w.agent.id,
      tab: (w.kind === 'input' ? 'terminal' : 'messages') as AgentTab,
      text: w.kind === 'plan' ? `${w.agent.name} sent a plan` : w.kind === 'question' ? `${w.agent.name} asked a question` : `${w.agent.name} is waiting in its terminal`,
    }))
  })
}

/** Moves to the next thing that waits on the user (the first when none of them is open). */
function goToNextWaiting(app: AppModel): void {
  const list = waitingTargets(app)
  if (list.length === 0) return
  const { hostId, agentId } = app.store.getState()
  const at = list.findIndex((t) => t.hostId === hostId && t.agentId === agentId)
  const t = list[(at + 1) % list.length]!
  app.selectAgent(t.hostId, t.agentId, t.tab)
}

/** The commands that have a shortcut of their own, by the id of their `app.keys.*` setting. */
export function keyedCommands(app: AppModel): Record<KeyedCommand, () => void> {
  const st = () => app.store.getState()
  return {
    palette: () => app.openPalette(!st().paletteOpen),
    settings: () => app.openSettings(!st().settingsOpen),
    dock: () => app.setDock(!st().dockOpen),
    tab_next: () => {
      const t = nextTab(st(), 1)
      if (t) app.selectAgent(t.hostId, t.agentId)
    },
    tab_prev: () => {
      const t = nextTab(st(), -1)
      if (t) app.selectAgent(t.hostId, t.agentId)
    },
    tab_close: () => {
      const { hostId, agentId } = st()
      if (hostId && agentId) app.closeTab({ hostId, agentId })
    },
    waiting_next: () => goToNextWaiting(app),
  }
}

/** Every command the palette lists, for the state the app is in now. */
export function buildCommands(app: AppModel): Command[] {
  const s = app.store.getState()
  const keyed = keyedCommands(app)
  const out: Command[] = []
  const selected = s.hosts.find((h) => h.info.id === s.hostId)
  const many = s.hosts.length > 1

  const view = (id: KeyedCommand, title: string, when = true): void => {
    if (when) out.push({ id, title, group: 'View', key: id, run: keyed[id] })
  }
  view('settings', s.settingsOpen ? 'Close the host settings' : 'Open the host settings', !!selected)
  view('dock', s.dockOpen ? 'Hide the live dock' : 'Show the live dock')
  view('tab_next', 'Next tab', s.tabs.length > 1)
  view('tab_prev', 'Previous tab', s.tabs.length > 1)
  view('tab_close', 'Close the open tab', !!s.agentId)
  const waiting = waitingTargets(app)
  view('waiting_next', 'Go to the next thing waiting on me', waiting.length > 0)
  if (selected && canPair(selected.store)) out.push({ id: 'pair', title: 'Pair a device', group: 'View', run: () => app.openPairing(true) })
  out.push({ id: 'shortcuts', title: 'Keyboard shortcuts…', group: 'View', detail: 'rebind keys', run: () => app.openSettings(true, 'app.keys') })

  if (selected) {
    out.push({
      id: 'start-run',
      title: 'Start a run…',
      group: 'Actions',
      detail: selected.info.name,
      ask: { prompt: `Title of the new run on ${selected.info.name}`, placeholder: 'What the run is for' },
      run: async (title = '') => {
        await startRun(selected.client, title.trim())
      },
    })
  }

  for (const w of waiting) {
    out.push({ id: `wait:${w.hostId}/${w.agentId}`, title: w.text, group: 'Waiting on you', run: () => app.selectAgent(w.hostId, w.agentId, w.tab) })
  }

  for (const h of s.hosts) {
    const hs = h.store.getState()
    if (many) out.push({ id: `host:${h.info.id}`, title: `Go to host ${h.info.name}`, group: 'Hosts', run: () => app.selectHost(h.info.id) })
    for (const run of Object.values(hs.runs).sort((a, b) => Number(a.status === RunStatus.CLOSED) - Number(b.status === RunStatus.CLOSED) || a.title.localeCompare(b.title))) {
      const lead = Object.values(hs.agents).find((a) => a.runId === run.id && a.role === AgentRole.ROOT)
      out.push({
        id: `run:${h.info.id}/${run.id}`,
        title: `Go to run ${run.title || run.id}`,
        group: 'Runs',
        detail: [run.id, many ? h.info.name : '', run.status === RunStatus.CLOSED ? 'closed' : ''].join(' '),
        run: () => (lead ? app.selectAgent(h.info.id, lead.id) : app.selectHost(h.info.id)),
      })
    }
    for (const a of Object.values(hs.agents).sort((x, y) => x.name.localeCompare(y.name))) {
      out.push(...agentCommands(app, h, a, many, hs.runs[a.runId]?.title ?? '', waiting.some((w) => w.hostId === h.info.id && w.agentId === a.id && w.tab === 'messages')))
    }
  }
  return out
}

function agentCommands(app: AppModel, h: HostRuntime, a: Agent, many: boolean, runTitle: string, hasPlanOrQuestion: boolean): Command[] {
  const detail = [ROLE_LABEL[a.role], statusSentence(a), runTitle, a.repo.split('/').pop(), many ? h.info.name : ''].filter(Boolean).join(' · ')
  const cmds: Command[] = [{ id: `agent:${h.info.id}/${a.id}`, title: `Go to ${a.name}`, group: 'Agents', detail, run: () => app.selectAgent(h.info.id, a.id) }]
  if (!alive(a)) return cmds
  const on = many ? ` (${h.info.name})` : ''
  const act = (verb: string, run: (c: HostRuntime['client'], id: string) => Promise<unknown>, extra: Partial<Command> = {}): Command => ({
    id: `${verb.toLowerCase()}:${h.info.id}/${a.id}`,
    title: `${verb} ${a.name}${on}`,
    group: 'Actions',
    detail,
    run: () => run(h.client, a.id),
    ...extra,
  })
  cmds.push(act('Ring', ringAgent))
  cmds.push(act('Retire', retireAgent, { confirm: `Retire ${a.name}? Its process stops and its conversation is kept; a message brings it back.` }))
  cmds.push(act('Stop', stopAgent, { confirm: `Stop ${a.name}? Its process ends.` }))
  if (a.state === AgentState.LIMITED) cmds.push(act('Switch', switchToNextModel, { title: `Move ${a.name} to its next model${on}` }))
  if (hasPlanOrQuestion) cmds.push(act('Approve', approveAgent, { title: `Approve the plan of ${a.name}${on}`, confirm: `Approve ${a.name}'s plan? It goes on at its execution tier.` }))
  return cmds
}

/** The commands matching a filter: every word of it in the title or the detail, titles first. */
export function filterCommands(cmds: Command[], query: string, limit = 60): Command[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean)
  if (words.length === 0) return cmds.slice(0, limit)
  const scored: Array<[number, Command]> = []
  for (const c of cmds) {
    const title = c.title.toLowerCase()
    const hay = `${title} ${c.group.toLowerCase()} ${(c.detail ?? '').toLowerCase()}`
    if (!words.every((w) => hay.includes(w))) continue
    const score = words.reduce((n, w) => n + (title.startsWith(w) ? 0 : title.includes(` ${w}`) ? 1 : title.includes(w) ? 2 : 4), 0)
    scored.push([score, c])
  }
  return scored.sort((a, b) => a[0] - b[0]).slice(0, limit).map(([, c]) => c)
}

/** The chord that runs a command, if it has one bound (from the first host's settings, as the shortcuts use them). */
export function chordOf(app: AppModel, c: Command): string {
  if (!c.key) return ''
  const first = app.store.getState().hosts[0]
  return appKeys(first?.store.getState().settings ?? {})[c.key] ?? DEFAULT_KEYS[c.key]
}
