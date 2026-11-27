import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, openFromSidebar, type App } from './electron'
import { buildBinaries, makeHost, until, type Host } from './harness'

let host: Host
let app: App
let bins: string
const shots = path.resolve(import.meta.dirname, '../test-results')

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  host = makeHost(bins)
  await host.start()
  mkdirSync(shots, { recursive: true })
  app = await launchApp({ state: host.state, home: host.home })
})

afterAll(async () => {
  await app?.stop()
  await host?.dispose()
  if (bins) rmSync(bins, { recursive: true, force: true })
})

test('a project shell: + › New terminal, typing, close and keep it running, open it again, survive the daemon, end it', async () => {
  const { page } = app
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  // The project exists once an agent works in it: a lead, opened from the sidebar, gives the tab bar its project.
  const { run } = await host.client().startRun({ title: 'shell run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await openFromSidebar(page, 'repo-lead')
  await page.getByRole('article', { name: 'repo-lead' }).waitFor()
  await page.getByRole('button', { name: 'Hide the side panel' }).click().catch(() => {})

  const tabs = page.getByRole('tablist', { name: 'Open agents' })
  await page.getByRole('button', { name: 'New in repo' }).click()
  await page.getByRole('menuitem', { name: 'New terminal' }).click()
  const tab = tabs.getByRole('tab', { name: 'Terminal' })
  await tab.waitFor()
  const panel = page.getByRole('article', { name: 'Terminal in repo' })
  const status = panel.getByTestId('terminal-status')
  const screen = panel.locator('.xterm-rows')
  await expect.poll(() => status.innerText(), { timeout: 20_000 }).toBe('Live')
  await panel.getByTestId('terminal').click()
  await page.keyboard.type('echo cwd=$(pwd) sum=$((6*7))')
  await page.keyboard.press('Enter')
  await expect.poll(() => screen.innerText(), { timeout: 20_000 }).toContain(`cwd=${host.repo} sum=42`)
  await page.screenshot({ path: path.join(shots, 'project-shell.png') })

  // The host lists it, and has no agent for it.
  expect((await host.client().listShells({})).shells).toHaveLength(1)

  // Closing the tab asks; Keep running closes the tab and leaves the shell, which the + menu opens again with its screen.
  await tabs.getByRole('button', { name: 'Close Terminal' }).click()
  await page.getByRole('dialog', { name: 'Close this terminal?' }).waitFor()
  await page.getByRole('button', { name: 'Keep running' }).click()
  await tab.waitFor({ state: 'detached' })
  expect((await host.client().listShells({})).shells).toHaveLength(1)
  await page.getByRole('button', { name: 'New in repo' }).click()
  await page.getByRole('menuitem', { name: 'Open the running terminal' }).click()
  await tabs.getByRole('tab', { name: 'Terminal' }).waitFor()
  await expect.poll(() => status.innerText(), { timeout: 20_000 }).toBe('Live')
  await expect.poll(() => screen.innerText(), { timeout: 10_000 }).toContain('sum=42')

  // The daemon restarts: the term host keeps the shell, and the tab carries on.
  await host.stopDaemon()
  await expect.poll(() => status.innerText(), { timeout: 20_000 }).toMatch(/^Reconnecting/)
  await host.start()
  await expect.poll(() => status.innerText(), { timeout: 30_000 }).toBe('Live')
  await panel.getByTestId('terminal').click()
  await page.keyboard.type('echo again-$((20+1))')
  await page.keyboard.press('Enter')
  await expect.poll(() => screen.innerText(), { timeout: 20_000 }).toContain('again-21')

  // Ending it from the close question stops the shell on the host.
  await tabs.getByRole('button', { name: 'Close Terminal' }).click()
  await page.getByRole('button', { name: 'End shell' }).click()
  await tabs.getByRole('tab', { name: 'Terminal' }).waitFor({ state: 'detached' })
  await until('the shell gone from the host', async () => (await host.client().listShells({})).shells.length === 0)
})
