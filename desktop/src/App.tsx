import { LazyMotion, MotionConfig, domAnimation } from 'motion/react'
import { useEffect, useMemo, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { TooltipProvider } from '@/components/ui/tooltip'
import { AppContext, useApp, useAppState, useHostState, useSelectedHost } from '@/context'
import { createAppModel, type HostRuntime } from '@/model'
import type { DesktopApi, OpenedLink } from '../shared/api'
import { AgentPanel } from '@/components/agent-panel'
import { ConnectionBanner } from '@/components/connection-banner'
import { CommandPalette } from '@/components/command-palette'
import { Dock } from '@/components/dock'
import { DropEdges, Panes } from '@/components/panes'
import { SidePanel } from '@/components/side-panel'
import { WatchAll } from '@/components/watch-all'
import { TabBar } from '@/components/tab-bar'
import { ViewToasts } from '@/components/view-toasts'
import { AddHostDialog, AddHostForm } from '@/components/host-dialogs'
import { LocalSetup } from '@/components/local-setup'
import { EmptyPane } from '@/components/empty-pane'
import { ProjectSidebar } from '@/components/project-sidebar'
import { FindBar } from '@/components/find-bar'
import { ProjectSettingsDialog } from '@/components/project-settings'
import { NewWorkspaceDialog } from '@/components/workspace-dialog'
import { PairPanel } from '@/components/pair-panel'
import { SettingsPanel } from '@/components/settings-panel'
import { UsagePanel } from '@/components/usage-panel'
import { useShortcuts } from '@/shortcuts'
import { appPrefs } from '@/prefs'
import { applyTheme, themeByName } from '@/theme'

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
      {/* Motion answers the user's actions only (rows and tiles coming and going), and none when they prefer reduced motion. */}
      <MotionConfig reducedMotion="user">
        <LazyMotion features={domAnimation} strict>
          <TooltipProvider delay={400}>
            <Shell opened={opened} linkChecked={linkChecked} onOpenedDone={() => setOpened(undefined)} />
          </TooltipProvider>
        </LazyMotion>
      </MotionConfig>
    </AppContext.Provider>
  )
}

function Shell({ opened, linkChecked, onOpenedDone }: { opened?: OpenedLink; linkChecked: boolean; onOpenedDone: () => void }) {
  const loaded = useAppState((s) => s.loaded)
  const error = useAppState((s) => s.error)
  const hosts = useAppState((s) => s.hosts)
  const host = useSelectedHost()
  const fixed = useApp().api.hosts.fixed
  useShortcuts()
  if (!loaded) return <ShellSkeleton />
  if (error) return <div role="alert" className="grid h-screen place-items-center text-destructive">{error}</div>
  if (hosts.length === 0) return <Welcome opened={opened} linkChecked={linkChecked} onDone={onOpenedDone} />
  return (
    <div className="flex h-screen">
      <ProjectSidebar />
      {host && (
        <>
          <ThemeSync host={host} />
          <main className="flex min-w-0 flex-1 flex-col">
            <ConnectionBanner host={host} />
            <TabBar />
            <div className="relative flex min-h-0 flex-1 flex-col">
              <MainPane host={host} />
              <DropEdges />
              <FindBar />
            </div>
            <Dock />
          </main>
          <PanelSlot host={host} />
          <CommandPalette />
          <ViewToasts />
          <WorkspaceSlot host={host} />
          <ProjectSettingsDialog />
        </>
      )}
      {/* A pairing link the operating system opened the app with: the add-a-host form, filled in. */}
      {opened && !fixed && <AddHostDialog open={false} onOpenChange={() => {}} opened={opened} onClose={onOpenedDone} />}
    </div>
  )
}

/** The window's outline while the saved hosts are read: a sidebar of rows and an empty main area. */
function ShellSkeleton() {
  return (
    <div role="status" aria-label="Loading" className="flex h-screen"><span className="sr-only">Loading</span>
      <div className="flex w-72 flex-col gap-3 border-r bg-surface p-3">
        <Skeleton className="h-4 w-1/2" />
        {[0, 1, 2, 3, 4].map((i) => <Skeleton key={i} className="h-9" />)}
      </div>
      <div className="flex-1" />
    </div>
  )
}

/** The New workspace dialog, on the host it was opened for (a project's host), else the selected one. */
function WorkspaceSlot({ host }: { host: HostRuntime }) {
  const on = useAppState((s) => s.hosts.find((h) => h.info.id === s.workspaceHost)) ?? host
  return <NewWorkspaceDialog key={on.info.id} host={on} />
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
          <h1 className="text-lg font-semibold">Connect to agent-mesh</h1>
          <p className="text-[13px] text-muted-foreground">
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
