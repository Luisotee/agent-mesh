import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, type App, openFromSidebar } from './electron'
import { buildBinaries, makeHost, until, type Host } from './harness'

let host: Host
let app: App | undefined
let bins: string
const shots = path.resolve(import.meta.dirname, '../test-results')

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  host = makeHost(bins)
  await host.start()
  mkdirSync(shots, { recursive: true })
})

afterAll(async () => {
  await app?.stop()
  await host?.dispose()
  if (bins) rmSync(bins, { recursive: true, force: true })
})

test('the open tabs, a split and the folded sidebar come back after the app restarts', async () => {
  const { run } = await host.client().startRun({ title: 'layout run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the lead', async () => (await host.client().listAgents({})).agents.length === 1)
  const lead = (await host.client().listAgents({})).agents[0]!
  await host.client(host.agentToken(lead.id)).spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })

  app = await launchApp({ state: host.state, home: host.home })
  let { page } = app
  await openFromSidebar(page, 'repo-fix')
  await openFromSidebar(page, 'repo-lead')
  const tabs = page.getByRole('tablist', { name: 'Open agents' })
  await tabs.getByRole('tab', { name: /repo-fix/ }).waitFor()
  // The lead on its chat (its view, unless the toggle showed its terminal).
  const leadView = page.getByRole('article', { name: 'repo-lead' })
  if (await leadView.getByRole('region', { name: 'Terminal' }).isVisible()) await page.getByRole('group', { name: 'View' }).getByRole('button', { name: 'Chat' }).click()
  // Show the worker beside the lead, and fold the sidebar to icons.
  await tabs.getByRole('tab', { name: /repo-fix/ }).click({ button: 'right' })
  await page.getByRole('menuitem', { name: 'Show beside' }).click()
  await page.getByRole('region', { name: 'Split pane' }).getByRole('article', { name: 'repo-fix' }).waitFor()
  await page.getByRole('button', { name: 'Fold the sidebar' }).click()
  await page.screenshot({ path: path.join(shots, 'layout-before-restart.png') })
  // The layout is written a moment after it changes.
  await new Promise((r) => setTimeout(r, 800))
  await app.stop()

  app = await launchApp({ state: host.state, home: host.home })
  ;({ page } = app)
  await page.getByRole('region', { name: 'Split pane' }).getByRole('article', { name: 'repo-fix' }).waitFor({ timeout: 20_000 })
  await page.getByRole('region', { name: 'Main pane' }).getByRole('article', { name: 'repo-lead' }).waitFor()
  expect(await page.getByRole('tablist', { name: 'Open agents' }).getByRole('tab').count()).toBe(2)
  // The lead was on its chat view.
  await page.getByRole('region', { name: 'Main pane' }).getByRole('region', { name: 'Chat' }).waitFor({ state: 'visible' })
  await page.getByRole('button', { name: 'Show the sidebar' }).waitFor()
  await page.screenshot({ path: path.join(shots, 'layout-after-restart.png') })
})
