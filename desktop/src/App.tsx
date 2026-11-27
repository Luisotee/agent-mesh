import { Gauge, PanelRight, QrCode, Search, Settings } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { Button } from '@/components/ui/button'
import { AppContext, useApp, useAppState, useHostState, useSelectedHost } from '@/context'
import { createAppModel, type HostRuntime } from '@/model'
import type { DesktopApi, OpenedLink } from '../shared/api'
import { AgentPanel } from '@/components/agent-panel'
import { ConnectionBanner } from '@/components/connection-banner'
import { CommandPalette } from '@/components/command-palette'
import { Dock } from '@/components/dock'
import { Panes } from '@/components/panes'
import { SidePanel } from '@/components/side-panel'
import { WatchAll } from '@/components/watch-all'
import { TabBar } from '@/components/tab-bar'
import { AddHostDialog, AddHostForm, RemoveHostDialog } from '@/components/host-dialogs'
import { LocalSetup } from '@/components/local-setup'
import { EmptyPane } from '@/components/empty-pane'
import { RunSidebar } from '@/components/run-sidebar'
import { PairPanel, canPair } from '@/components/pair-panel'
import { SettingsPanel } from '@/components/settings-panel'
import { UsagePanel } from '@/components/usage-panel'
import { joinWaiting } from '@/derive'
import { useShortcuts } from '@/shortcuts'
import { appPrefs } from '@/prefs'
import { applyTheme, themeByName } from '@/theme'
import { cn } from '@/lib/utils'

export function App({ api }: { api: DesktopApi }) {
  const model = useMemo(() => createAppModel(api), [api])
  useEffect(() => {
    void model.load()
  }, [model])
  // A pairing link the operating system opened the app with: shown in the add-a-host form, never acted on. With the
  // host list fixed by the environment no host can be added, so there is nothing to show it in.
  const [opened, setOpened] = useState<OpenedLink>()
  // Whether the link that may have started the app was looked for yet: the first-run screen waits for it, so it doesn't go
  // and connect to a daemon here while the user came to add the host a link names.
  const [linkChecked, setLinkChecked] = useState(api.hosts.fixed)
  useEffect(() => {
    if (api.hosts.fixed) return
    // The link is handed over once: a result is kept even if this effect ran again meanwhile (React's strict mode runs it twice).
    void api.links
      .take()
      .then((l) => l && setOpened(l))
      .catch(() => {})
      .finally(() => setLinkChecked(true))
    return api.links.onOpen(setOpened)
  }, [api])
  return (
    <AppContext.Provider value={model}>
      <Shell opened={opened} linkChecked={linkChecked} onOpenedDone={() => setOpened(undefined)} />
    </AppContext.Provider>
  )
}

function Shell({ opened, linkChecked, onOpenedDone }: { opened?: OpenedLink; linkChecked: boolean; onOpenedDone: () => void }) {
  const loaded = useAppState((s) => s.loaded)
  const error = useAppState((s) => s.error)
  const hosts = useAppState((s) => s.hosts)
  const host = useSelectedHost()
  useShortcuts()
  if (!loaded) return <div className="grid h-screen place-items-center text-muted-foreground">Loading…</div>
  if (error) return <div role="alert" className="grid h-screen place-items-center text-destructive">{error}</div>
  if (hosts.length === 0) return <Welcome opened={opened} linkChecked={linkChecked} onDone={onOpenedDone} />
  return (
    <div className="flex h-screen">
      <HostRail opened={opened} onOpenedDone={onOpenedDone} />
      {host && (
        <>
          <ThemeSync host={host} />
          <aside className="flex w-80 shrink-0 flex-col border-r bg-surface">
            <HostHeader host={host} />
            <RunSidebar host={host} />
          </aside>
          <main className="flex min-w-0 flex-1 flex-col">
            <ConnectionBanner host={host} />
            <TabBar />
            <MainPane host={host} />
            <Dock />
          </main>
          <PanelSlot host={host} />
          <CommandPalette />
        </>
      )}
    </div>
  )
}

/** The side panel beside the open agent, when it is shown. */
function PanelSlot({ host }: { host: HostRuntime }) {
  const open = useAppState((s) => s.panelOpen)
  const agentId = useAppState((s) => (s.hostId === host.info.id ? s.agentId : undefined))
  return open ? <SidePanel host={host} agentId={agentId} /> : null
}

/** The look follows the selected host's `app.theme` (Dracula until it says, or when it names a theme this build lacks). */
function ThemeSync({ host }: { host: HostRuntime }) {
  // Until the host's settings arrive its theme isn't known: keep the look as it is rather than flash Dracula.
  const name = useHostState(host, (s) => (Object.keys(s.settings).length > 0 ? appPrefs(s.settings).theme : undefined))
  useEffect(() => {
    if (name !== undefined) applyTheme(themeByName(name))
  }, [name])
  return null
}

function Welcome({ opened, linkChecked, onDone }: { opened?: OpenedLink; linkChecked: boolean; onDone: () => void }) {
  const app = useApp()
  // A pairing link that opened the app is what the user came to do: its form first. Otherwise the app looks for a daemon here.
  const [manual, setManual] = useState(!!opened)
  // A link that arrives while this screen shows (the app was already running) opens the form too.
  useEffect(() => {
    if (opened) setManual(true)
  }, [opened])
  return (
    <div className="grid h-screen place-items-center overflow-y-auto p-6">
      <div className="flex w-[34rem] max-w-full flex-col gap-4 rounded-lg border bg-card p-6">
        <div>
          <h1 className="text-base font-semibold">Connect to agent-mesh</h1>
          <p className="text-xs text-muted-foreground">
            agent-mesh runs as a daemon on your computer (on Windows, inside WSL) or on a server. The app looks for one on this computer first.
          </p>
        </div>
        {app.api.hosts.fixed ? (
          <p role="alert" className="text-xs text-destructive">The host is set by the environment but isn't available.</p>
        ) : (
          <>
            {!opened && linkChecked && <LocalSetup auto />}
            {manual ? (
              <div className="flex flex-col gap-2 border-t pt-4">
                <h2 className="text-sm font-medium">Connect another way</h2>
                <p className="text-xs text-muted-foreground">
                  A daemon on another machine: make a pairing link there (its "Pair a device" screen, or <code className="font-mono">mesh pair</code>). A daemon here whose state folder isn't the usual one: give that folder.
                </p>
                <AddHostForm key={opened?.link} opened={opened} onDone={onDone} />
              </div>
            ) : (
              <div>
                <Button variant="outline" size="sm" onClick={() => setManual(true)}>Connect another way</Button>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  )
}

function HostRail({ opened, onOpenedDone }: { opened?: OpenedLink; onOpenedDone: () => void }) {
  const app = useApp()
  const hosts = useAppState((s) => s.hosts)
  const selected = useAppState((s) => s.hostId)
  return (
    <nav aria-label="Hosts" className="flex w-14 shrink-0 flex-col items-center gap-2 border-r bg-background py-3">
      {hosts.map((h) => (
        <RailItem key={h.info.id} host={h} selected={h.info.id === selected} onSelect={() => app.selectHost(h.info.id)} />
      ))}
      {!app.api.hosts.fixed && <AddHostDialog opened={opened} onClose={onOpenedDone} />}
    </nav>
  )
}

const DOT: Record<string, string> = { ready: 'bg-state-done', connecting: 'bg-state-needs', reconnecting: 'bg-state-needs', stopped: 'bg-state-failed' }

function RailItem({ host, selected, onSelect }: { host: HostRuntime; selected: boolean; onSelect: () => void }) {
  const kind = useHostState(host, (s) => s.conn.kind)
  const waiting = useHostState(host, (s) => joinWaiting(s.waiting, s.runs, s.agents).length)
  return (
    <button
      type="button"
      onClick={onSelect}
      title={`${host.info.name}: ${kind}`}
      aria-label={`Host ${host.info.name}, ${kind}`}
      aria-current={selected ? 'true' : undefined}
      className={cn('relative grid size-9 place-items-center rounded-lg border bg-card text-sm font-semibold uppercase hover:bg-accent', selected && 'border-primary')}
    >
      {host.info.name.slice(0, 2)}
      <span className={cn('absolute -right-0.5 -bottom-0.5 size-2.5 rounded-full ring-2 ring-background', DOT[kind])} />
      {waiting > 0 && <span className="absolute -top-1 -right-1 grid min-w-4 place-items-center rounded-full bg-state-needs px-1 text-[10px] leading-4 font-bold text-primary-foreground">{waiting}</span>}
    </button>
  )
}

function HostHeader({ host }: { host: HostRuntime }) {
  const app = useApp()
  const conn = useHostState(host, (s) => s.conn)
  const open = useAppState((s) => s.settingsOpen)
  const pairOpen = useAppState((s) => s.pairOpen)
  const panelOpen = useAppState((s) => s.panelOpen)
  const usageOpen = useAppState((s) => s.usageOpen)
  // Only the host's own device may pair devices, so only there is the screen offered.
  const mayPair = useHostState(host, () => canPair(host.store))
  return (
    <header className="flex items-start gap-2 border-b px-3 py-2.5">
      <div className="min-w-0 flex-1">
        <h1 className="truncate text-sm font-semibold">{host.info.name}</h1>
        <p className="truncate text-[11px] text-muted-foreground">
          {conn.kind === 'ready' ? `daemon ${conn.hello.version} · you are ${conn.hello.actor?.name || 'a device'}` : conn.kind}
        </p>
      </div>
      <Button size="icon" variant="ghost" aria-label="Command palette" title="Command palette" onClick={() => app.openPalette(true)}>
        <Search />
      </Button>
      <RemoveHostDialog host={host} />
      {mayPair && (
        <Button size="icon" variant={pairOpen ? 'secondary' : 'ghost'} aria-label="Pair a device" aria-pressed={pairOpen} onClick={() => app.openPairing(!pairOpen)}>
          <QrCode />
        </Button>
      )}
      <Button size="icon" variant={panelOpen ? 'secondary' : 'ghost'} aria-label="Side panel" aria-pressed={panelOpen} title="Cost, pull requests and quota of the open agent's run" onClick={() => app.setPanel(!panelOpen)}>
        <PanelRight />
      </Button>
      <Button size="icon" variant={usageOpen ? 'secondary' : 'ghost'} aria-label="Usage and cost" aria-pressed={usageOpen} onClick={() => app.openUsage(!usageOpen)}>
        <Gauge />
      </Button>
      <Button size="icon" variant={open ? 'secondary' : 'ghost'} aria-label="Settings" aria-pressed={open} onClick={() => app.openSettings(!open)}>
        <Settings />
      </Button>
    </header>
  )
}

function MainPane({ host }: { host: HostRuntime }) {
  const settingsOpen = useAppState((s) => s.settingsOpen)
  const pairOpen = useAppState((s) => s.pairOpen)
  const usageOpen = useAppState((s) => s.usageOpen)
  const agentId = useAppState((s) => (s.hostId === host.info.id ? s.agentId : undefined))
  const side = useAppState((s) => s.side)
  const watchOpen = useAppState((s) => s.watchOpen)
  if (settingsOpen) return <SettingsPanel host={host} />
  if (pairOpen) return <PairPanel key={host.info.id} host={host} />
  if (usageOpen) return <UsagePanel host={host} />
  if (watchOpen) return <WatchAll />
  if (side.length > 0) return <Panes host={host} agentId={agentId} />
  if (!agentId) return <EmptyPane host={host} />
  return <AgentPanel key={agentId} host={host} agentId={agentId} />
}
