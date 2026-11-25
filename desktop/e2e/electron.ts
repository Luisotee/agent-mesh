import { type ChildProcess, spawn } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import path from 'node:path'
import { chromium, type Browser, type Page } from 'playwright-core'

const desktopDir = path.resolve(import.meta.dirname, '..')

export interface App {
  /** What the main process was asked to do for notifications and the tray (it shows no notification in a test). */
  shellLog: string
  page: Page
  browser: Browser
  proc: ChildProcess
  stop(): Promise<void>
}

/**
 * Starts the built app with a state dir for its one host and a user-data dir of its own, and drives it over the
 * Chrome DevTools Protocol. `--no-sandbox` is for the test machine only (WSL has no SUID helper for Chromium).
 */
export async function launchApp(opts: { state: string; home: string }): Promise<App> {
  const userData = path.join(opts.home, 'userData')
  const shellLog = path.join(opts.home, 'shell.log')
  mkdirSync(userData, { recursive: true })
  const electron = path.join(desktopDir, 'node_modules/.bin/electron')
  const proc = spawn(electron, ['.', '--no-sandbox', '--remote-debugging-port=0'], {
    cwd: desktopDir,
    env: {
      ...process.env,
      HOME: opts.home,
      XDG_CONFIG_HOME: path.join(opts.home, '.config'),
      XDG_CACHE_HOME: path.join(opts.home, '.cache'),
      XDG_DATA_HOME: path.join(opts.home, '.local/share'),
      AGENT_MESH_DESKTOP_STATE: opts.state,
      AGENT_MESH_DESKTOP_USERDATA: userData,
      AGENT_MESH_DESKTOP_SHELL_LOG: shellLog,
      ELECTRON_ENABLE_LOGGING: '1',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  const ws = await new Promise<string>((resolve, reject) => {
    let log = ''
    const timer = setTimeout(() => reject(new Error(`electron printed no DevTools address:\n${log}`)), 30_000)
    const onData = (b: Buffer) => {
      log += b.toString()
      const m = /DevTools listening on (ws:\/\/\S+)/.exec(log)
      if (m) {
        clearTimeout(timer)
        resolve(m[1]!)
      }
    }
    proc.stderr?.on('data', onData)
    proc.stdout?.on('data', onData)
    proc.on('exit', (code) => reject(new Error(`electron exited (${code}) before it was ready:\n${log}`)))
  })
  const browser = await chromium.connectOverCDP(ws)
  const context = browser.contexts()[0]!
  const page = context.pages()[0] ?? (await context.waitForEvent('page'))
  await page.waitForLoadState('domcontentloaded')
  return {
    shellLog,
    page,
    browser,
    proc,
    async stop() {
      await browser.close().catch(() => {})
      proc.kill('SIGTERM')
      await new Promise((r) => (proc.exitCode != null ? r(null) : proc.once('exit', r)))
    },
  }
}
