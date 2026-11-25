import { createContext, useContext } from 'react'
import { useStore } from 'zustand'
import type { AppModel, AppState, HostRuntime } from './model'
import type { HostState } from './store'

export const AppContext = createContext<AppModel | undefined>(undefined)

export function useApp(): AppModel {
  const m = useContext(AppContext)
  if (!m) throw new Error('no AppContext')
  return m
}

export function useAppState<T>(select: (s: AppState) => T): T {
  return useStore(useApp().store, select)
}

export function useHostState<T>(rt: HostRuntime, select: (s: HostState) => T): T {
  return useStore(rt.store, select)
}

export function useSelectedHost(): HostRuntime | undefined {
  return useAppState((s) => s.hosts.find((h) => h.info.id === s.hostId))
}
