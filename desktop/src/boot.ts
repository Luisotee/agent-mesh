import { StrictMode, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import type { DesktopApi } from '../shared/api'
import { App } from './App'
import { DEFAULT_THEME, applyTheme, themeByName } from './theme'

/**
 * Starts the app in `root`. A failure while it starts is written into the page, as text: a window that stays blank
 * and says nothing (a build with no theme files was one) leaves the user nothing to report.
 */
export function boot(root: HTMLElement, api: DesktopApi): void {
  try {
    applyTheme(themeByName(DEFAULT_THEME))
    createRoot(root).render(createElement(StrictMode, null, createElement(App, { api })))
  } catch (e) {
    const pre = document.createElement('pre')
    pre.setAttribute('role', 'alert')
    pre.style.cssText = 'margin:0;padding:16px;white-space:pre-wrap;font:13px monospace;color:#ff5555;background:#282a36'
    pre.textContent = `agent-mesh could not start: ${e instanceof Error ? e.message : String(e)}`
    root.replaceChildren(pre)
    throw e
  }
}
