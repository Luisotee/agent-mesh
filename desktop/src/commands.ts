import { approveAgent, renameRun, retireAgent, ringAgent, startRun, stopAgent, switchToNextModel } from './actions'
import { canPair } from './components/pair-panel'
import { stepSidebar } from './components/project-sidebar'
import { TERMINAL_SEARCH } from './keys'
import { joinWaiting, ROLE_LABEL, statusSentence, type Waiting } from './derive'
import { AgentRole, AgentState, RunStatus, type Agent } from './gen/agentmesh/v1/types_pb'
import { shownTabs, type AgentTab, type AppModel, type HostRuntime } from './model'
import { DEFAULT_KEYS, appKeys, type KeyedCommand } from './prefs'
import { MAX_SIDE_PANES, cycleTab as nextTab } from './workspace'

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
export function waitingTargets(app: AppModel): Array<{ hostId: string; agentId: string; kind: Waiting['kind']; tab: AgentTab; text: string }> {
  return app.store.getState().hosts.flatMap((h) => {
    const s = h.store.getState()
    return joinWaiting(s.waiting, s.runs, s.agents).map((w) => ({
      hostId: h.info.id,
      agentId: w.agent.id,
      kind: w.kind,
      tab: (w.kind === 'input' ? 'terminal' : 'chat') as AgentTab,
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

/** Whether the open agent has a terminal to switch to (the agent has one and the host has terminals): the chat toggle's other side. */
function hasOpenTerminal(app: AppModel): boolean {
  const { hostId, agentId } = app.store.getState()
  const h = app.store.getState().hosts.find((x) => x.info.id === hostId)
  const s = h?.store.getState()
  return !!agentId && !!s?.daemon?.capabilities.includes('terminals') && !!s.agents[agentId]?.terminalId
}

/** The commands that have a shortcut of their own, by the id of their `app.keys.*` setting. */
export function keyedCommands(app: AppModel): Record<KeyedCommand, () => void> {
  const st = () => app.store.getState()
  const keyed: Record<KeyedCommand, () => void> = {
    palette: () => app.openPalette(!st().paletteOpen),
    settings: () => app.openSettings(!st().settingsOpen),
    dock: () => app.setDock(!st().dockOpen),
    chat_toggle: () => {
      const { agentId, tab } = st()
      // From the chat to the terminal only where there is one (an exited agent has none): the panel would show the
      // chat still, and the next press would seem to do nothing.
      if (agentId && (tab === 'chat' ? hasOpenTerminal(app) : true)) app.setTab(tab === 'chat' ? 'terminal' : 'chat')
    },
    tab_next: () => {
      const t = nextTab(st(), 1, shownTabs(st()))
      if (t) app.selectAgent(t.hostId, t.agentId)
    },
    tab_prev: () => {
      const t = nextTab(st(), -1, shownTabs(st()))
      if (t) app.selectAgent(t.hostId, t.agentId)
    },
    tab_next_page: () => keyed.tab_next(),
    tab_prev_page: () => keyed.tab_prev(),
    tab_close: () => {
      const { hostId, agentId } = st()
      if (hostId && agentId) app.closeTab({ hostId, agentId })
    },
    tab_new: () => {
      const { hostId, agentId, hosts } = st()
      const repo = hostId && agentId ? hosts.find((h) => h.info.id === hostId)?.store.getState().agents[agentId]?.repo : undefined
      app.openWorkspace(true, undefined, hostId && repo ? { hostId, repo } : undefined)
    },
    tab_reopen: () => app.reopenTab(),
    tab_1: () => goToTab(app, 0),
    tab_2: () => goToTab(app, 1),
    tab_3: () => goToTab(app, 2),
    tab_4: () => goToTab(app, 3),
    tab_5: () => goToTab(app, 4),
    tab_6: () => goToTab(app, 5),
    tab_7: () => goToTab(app, 6),
    tab_8: () => goToTab(app, 7),
    tab_last: () => goToTab(app, shownTabs(st()).length - 1),
    waiting_next: () => goToNextWaiting(app),
    search: () => app.openPalette(true),
    sidebar: () => app.setSidebarCollapsed(!st().sidebarCollapsed),
    sidebar_next: () => stepSidebar(1),
    sidebar_prev: () => stepSidebar(-1),
    panel: () => app.setPanel(!st().panelOpen),
    find: () => {
      // In a terminal, its own search (the window's find doesn't see xterm's canvas).
      const el = typeof document === 'undefined' ? null : document.activeElement
      if (el?.closest('.xterm')) return void el.dispatchEvent(new CustomEvent(TERMINAL_SEARCH, { bubbles: true }))
      app.openFind(!st().findOpen)
    },
    rename: () => {
      // The focused sidebar row's run, else the open agent's.
      const row = document.activeElement?.closest<HTMLElement>('[data-sidebar-row="agent"]')
      const { hostId, agentId } = row?.dataset.agentId ? { hostId: row.dataset.hostId, agentId: row.dataset.agentId } : st()
      const { hosts } = st()
      const runId = hostId && agentId ? hosts.find((h) => h.info.id === hostId)?.store.getState().agents[agentId]?.runId : undefined
      if (hostId && runId) {
        app.setSidebarCollapsed(false)
        app.renameRun({ hostId, runId })
      }
    },
  }
  return keyed
}

/** The tab at a place in the open project's tab bar (Mod+1…8, Mod+9 the last). */
function goToTab(app: AppModel, at: number): void {
  const t = shownTabs(app.store.getState())[at]
  if (t) app.selectAgent(t.hostId, t.agentId)
}

/** The host's settings by what they are for: a palette command opens the screen filtered to one (the query is a key prefix). */
const SETTINGS_SECTIONS: ReadonlyArray<readonly [title: string, query: string, detail: string]> = [
  ['Notifications', 'app.notify', 'alerts sound tray'],
  ['Theme', 'app.theme', 'colours appearance'],
  ['Model tiers', 'tiers.', 'models effort light mid strong'],
  ['What leads may do without asking', 'autonomy.', 'autonomy merge approve ask before'],
  ['Repositories', 'repos.', 'repos folder no free models'],
  ['Permission prompts', 'permission.', 'wait allow deny'],
]

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
  view('chat_toggle', 'Switch the open agent between chat and terminal', !!s.agentId && (s.tab !== 'chat' || hasOpenTerminal(app)))
  view('tab_next', 'Next tab', shownTabs(s).length > 1)
  view('tab_prev', 'Previous tab', shownTabs(s).length > 1)
  view('tab_close', 'Close the open tab', !!s.agentId)
  view('tab_reopen', 'Open the last closed tab again', s.closedTabs.length > 0)
  view('tab_new', 'New lead in this project', !!selected)
  view('sidebar', s.sidebarCollapsed ? 'Unfold the sidebar' : 'Fold the sidebar')
  view('panel', s.panelOpen ? 'Hide the side panel' : 'Show the side panel')
  view('find', 'Find in the window')
  view('rename', 'Rename the open run', !!s.agentId)
  const waiting = waitingTargets(app)
  view('waiting_next', 'Go to the next thing waiting on me', waiting.length > 0)
  out.push({ id: 'watch-all', title: s.watchOpen ? 'Close Watch all' : 'Watch all agents', group: 'View', detail: 'grid every live agent', run: () => app.openWatch(!s.watchOpen) })
  if (s.side.length > 0) {
    out.push({ id: 'split-dir', title: s.splitDir === 'row' ? 'Stack the panes' : 'Put the panes side by side', group: 'View', detail: 'split layout', run: () => app.setSplitDir(s.splitDir === 'row' ? 'column' : 'row') })
    out.push({ id: 'split-close', title: 'Close the splits', group: 'View', detail: 'panes beside', run: () => s.side.forEach((p) => app.unsplit(p)) })
  }
  if (selected) out.push({ id: 'usage', title: s.usageOpen ? 'Close Usage and cost' : 'Usage and cost', group: 'View', detail: 'quota spend tokens', run: () => app.openUsage(!s.usageOpen) })
  if (selected && canPair(selected.store)) out.push({ id: 'pair', title: 'Pair a device', group: 'View', run: () => app.openPairing(true) })
  out.push({ id: 'shortcuts', title: 'Keyboard shortcuts…', group: 'View', detail: 'rebind keys', run: () => app.openSettings(true, 'app.keys') })
  if (selected) {
    for (const [title, query, detail] of SETTINGS_SECTIONS) {
      out.push({ id: `settings:${query}`, title: `Settings: ${title}`, group: 'View', detail: `${detail} host settings`, run: () => app.openSettings(true, query) })
    }
  }

  if (selected) {
    out.push({ id: 'setup', title: 'Set up with the assistant', group: 'Actions', detail: `${selected.info.name} · connect repos, machines, your phone`, run: () => app.openSetup(selected.info.id) })
    out.push({
      id: 'remove-host',
      title: `Remove host ${selected.info.name}`,
      group: 'Hosts',
      detail: 'forget this host in the app',
      confirm: `Remove ${selected.info.name} from the app? Its saved token is deleted; the daemon and its agents are untouched.`,
      run: () => app.removeHost(selected.info.id),
    })
    out.push({ id: 'new-workspace', title: 'New workspace…', group: 'Actions', detail: `${selected.info.name} project worktree branch pull request`, run: () => app.openWorkspace(true) })
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
      if (run.status !== RunStatus.CLOSED) {
        out.push({
          id: `add-agent:${h.info.id}/${run.id}`,
          title: `Add an agent to run ${run.title || run.id}…`,
          group: 'Actions',
          detail: [run.id, many ? h.info.name : '', 'spawn worker workspace'].join(' '),
          run: () => {
            app.selectHost(h.info.id)
            app.openWorkspace(true, run.id)
          },
        })
      }
      out.push({
        id: `rename-run:${h.info.id}/${run.id}`,
        title: `Rename run ${run.title || run.id}…`,
        group: 'Actions',
        detail: [run.id, many ? h.info.name : ''].join(' '),
        ask: { prompt: `New title for run ${run.title || run.id}`, placeholder: run.title || 'What the run is for' },
        run: async (title = '') => {
          if (title.trim()) await renameRun(h.client, run.id, title.trim())
        },
      })
      out.push({
        id: `run:${h.info.id}/${run.id}`,
        title: `Go to run ${run.title || run.id}`,
        group: 'Runs',
        detail: [run.id, many ? h.info.name : '', run.status === RunStatus.CLOSED ? 'closed' : ''].join(' '),
        run: () => (lead ? app.selectAgent(h.info.id, lead.id) : app.selectHost(h.info.id)),
      })
    }
    for (const a of Object.values(hs.agents).sort((x, y) => x.name.localeCompare(y.name))) {
      out.push(...agentCommands(app, h, a, many, hs.runs[a.runId]?.title ?? '', waiting.some((w) => w.hostId === h.info.id && w.agentId === a.id && w.kind === 'plan')))
    }
  }
  return out
}

function agentCommands(app: AppModel, h: HostRuntime, a: Agent, many: boolean, runTitle: string, hasPlan: boolean): Command[] {
  const detail = [ROLE_LABEL[a.role], statusSentence(a), runTitle, a.repo.split('/').pop(), many ? h.info.name : ''].filter(Boolean).join(' · ')
  const cmds: Command[] = [{ id: `agent:${h.info.id}/${a.id}`, title: `Go to ${a.name}`, group: 'Agents', detail, run: () => app.selectAgent(h.info.id, a.id) }]
  const st = app.store.getState()
  const shown = (st.hostId === h.info.id && st.agentId === a.id) || st.side.some((p) => p.hostId === h.info.id && p.agentId === a.id)
  if (st.agentId && !shown && st.side.length < MAX_SIDE_PANES) cmds.push({ id: `split:${h.info.id}/${a.id}`, title: `Show ${a.name} beside the open agent`, group: 'Actions', detail, run: () => app.pinSplit(h.info.id, a.id) })
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
  cmds.push(act('Retire', retireAgent, { confirm: `Retire ${a.name}? Its process stops and its conversation is kept; a task or an answer brings it back.` }))
  cmds.push(act('Stop', stopAgent, { confirm: `Stop ${a.name}? Its process ends.` }))
  if (a.state === AgentState.LIMITED) cmds.push(act('Switch', switchToNextModel, { title: `Move ${a.name} to its next model${on}` }))
  if (hasPlan) cmds.push(act('Approve', approveAgent, { title: `Approve the plan of ${a.name}${on}`, confirm: `Approve ${a.name}'s plan? It goes on at its execution tier.` }))
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
