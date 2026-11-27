import { mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { afterAll, beforeAll, expect, test } from 'vitest'
import { launchApp, type App } from './electron'
import { buildBinaries, fakeWsl, freePort, makeHost, until, type Host } from './harness'

// The first run: the app has no host, finds the daemon in a (stand-in) WSL distro, pairs with it through `mesh pair` run
// inside it, and opens the setup agent's chat. The platform is told to be Windows and `wsl.exe` is a script that runs what
// is asked against a real meshd on a state dir of its own: nothing here touches the user's daemon or services.

let bins: string
let host: Host
let dir: string
const apps: App[] = []
const hosts: Host[] = []
const shots = path.resolve(import.meta.dirname, '../test-results')

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  dir = mkdtempSync(path.join(os.tmpdir(), 'amd-w-'))
  mkdirSync(shots, { recursive: true })
  host = makeHost(bins, { listen: `127.0.0.1:${await freePort()}`, hostName: 'wsl-box', share: true })
  hosts.push(host)
  await host.start()
}, 180_000)

afterAll(async () => {
  for (const a of apps) await a.stop()
  for (const h of hosts) await h.dispose()
  if (dir) rmSync(dir, { recursive: true, force: true })
  if (bins) rmSync(bins, { recursive: true, force: true })
})

async function launch(wsl: { exe: string }): Promise<App> {
  const home = mkdtempSync(path.join(os.tmpdir(), 'amd-h3-'))
  const app = await launchApp({ home, env: { AGENT_MESH_DESKTOP_PLATFORM: 'win32', AGENT_MESH_DESKTOP_WSL_EXE: wsl.exe } })
  apps.push(app)
  return app
}

test('a first run finds the daemon in WSL, pairs with it with nothing pasted, and opens the setup chat', async () => {
  const app = await launch(fakeWsl(path.join(dir, 'ok'), bins, host))
  const { page } = app
  // No click: the host is added and the setup agent's chat is the first screen.
  await page.getByRole('status', { name: /Host wsl-box/ }).waitFor({ timeout: 60_000 }).catch(async (e: unknown) => {
    throw new Error(`${String(e)}\nthe page says: ${(await page.locator('body').innerText()).slice(0, 800)}`)
  })
  await page.getByRole('article', { name: 'setup-agent' }).waitFor({ timeout: 60_000 })
  await page.getByRole('tabpanel', { name: 'chat' }).waitFor({ state: 'visible' })
  await page.screenshot({ path: path.join(shots, 'onboarding-chat.png') })

  // The host really has the app as a paired device, over the pinned TLS address, and a setup run with its lead.
  const c = host.client()
  await until('the paired device', async () => (await c.listDevices({})).devices.some((d) => d.name.startsWith('Desktop (')))
  const runs = (await c.listRuns({})).runs
  expect(runs.filter((r) => r.setup)).toHaveLength(1)
  const agents = (await c.listAgents({})).agents
  expect(agents.filter((a) => a.name === 'setup-agent')).toHaveLength(1)

  // Opening the setup again (the palette; the empty pane's button does the same) lands on the same agent: no second run.
  await page.getByRole('tab', { name: /setup-agent/ }).waitFor()
  await page.keyboard.press('Control+Shift+P')
  const palette = page.getByRole('combobox', { name: 'Search commands' })
  await palette.fill('set up with')
  await page.keyboard.press('Enter')
  await page.getByRole('article', { name: 'setup-agent' }).waitFor()
  expect((await c.listRuns({})).runs.filter((r) => r.setup)).toHaveLength(1)
  expect((await c.listAgents({})).agents.filter((a) => a.name === 'setup-agent')).toHaveLength(1)
})

test('a distro without agent-mesh gets what to run, and nothing is connected', async () => {
  const empty = makeHost(bins, { share: true })
  hosts.push(empty)
  await empty.start()
  const app = await launch(fakeWsl(path.join(dir, 'none'), bins, empty, { withMesh: false }))
  const { page } = app
  const card = page.getByTestId('local-wsl:Ubuntu')
  await card.waitFor({ timeout: 60_000 })
  expect(await card.textContent()).toContain("agent-mesh isn't installed in Ubuntu")
  expect(await card.textContent()).toContain('./install.sh')
  await page.screenshot({ path: path.join(shots, 'onboarding-not-installed.png') })
  // The manual form is still there as the fallback.
  await page.getByRole('button', { name: 'Connect another way' }).click()
  await page.getByLabel('State directory').waitFor()
  expect((await empty.client().listDevices({})).devices.filter((d) => d.name.startsWith('Desktop ('))).toHaveLength(0)
})

test('a daemon that is installed but only listens inside WSL offers the fix, says what it does, and runs it only on a click', async () => {
  const quiet = makeHost(bins, { share: true }) // no --listen
  hosts.push(quiet)
  await quiet.start()
  const wsl = fakeWsl(path.join(dir, 'quiet'), bins, quiet)
  const app = await launch(wsl)
  const { page } = app
  const card = page.getByTestId('local-wsl:Ubuntu')
  await card.waitFor({ timeout: 60_000 })
  expect(await card.textContent()).toContain('Not reachable from Windows')
  expect(await card.textContent()).toContain('127.0.0.1:9443')
  // Nothing ran yet: no service was touched.
  expect(() => readFileSync(wsl.systemctlLog, 'utf8')).toThrow()
  await card.getByRole('button', { name: 'Let Windows reach it' }).click()
  // The (stub) systemctl saw the reload and the restart; the drop-in is in the distro's home.
  await until('the restart', () => {
    try {
      return readFileSync(wsl.systemctlLog, 'utf8').includes('restart agent-mesh')
    } catch {
      return false
    }
  }, 30_000)
  expect(readFileSync(wsl.systemctlLog, 'utf8')).toContain('--user daemon-reload')
  expect(readFileSync(path.join(quiet.home, '.config/systemd/user/agent-mesh.service.d/listen.conf'), 'utf8')).toContain('AGENT_MESH_LISTEN=127.0.0.1:9443')
})
