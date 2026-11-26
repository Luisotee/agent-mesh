import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, type App } from './electron'
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

test('the open tabs, a split and the sidebar grouping come back after the app restarts', async () => {
  const { run } = await host.client().startRun({ title: 'layout run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the lead', async () => (await host.client().listAgents({})).agents.length === 1)
  const lead = (await host.client().listAgents({})).agents[0]!
  await host.client(host.agentToken(lead.id)).spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })

  app = await launchApp({ state: host.state, home: host.home })
  let { page } = app
  let nav = page.getByRole('navigation', { name: 'Runs' })
  await nav.getByRole('button', { name: /repo-fix/ }).click()
  await nav.getByRole('button', { name: /repo-lead/ }).click()
  const tabs = page.getByRole('tablist', { name: 'Open agents' })
  await tabs.getByRole('tab', { name: /repo-fix/ }).waitFor()
  await page.getByRole('tab', { name: 'chat' }).first().click()
  // Show the worker beside the lead, and group the sidebar by status.
  await tabs.getByRole('tab', { name: /repo-fix/ }).hover()
  await tabs.getByRole('button', { name: 'Show repo-fix beside' }).click()
  await page.getByRole('region', { name: 'Split pane' }).getByRole('article', { name: 'repo-fix' }).waitFor()
  await nav.getByRole('button', { name: 'Status' }).click()
  await page.screenshot({ path: path.join(shots, 'layout-before-restart.png') })
  // The layout is written a moment after it changes.
  await new Promise((r) => setTimeout(r, 800))
  await app.stop()

  app = await launchApp({ state: host.state, home: host.home })
  ;({ page } = app)
  nav = page.getByRole('navigation', { name: 'Runs' })
  await page.getByRole('region', { name: 'Split pane' }).getByRole('article', { name: 'repo-fix' }).waitFor({ timeout: 20_000 })
  await page.getByRole('region', { name: 'Main pane' }).getByRole('article', { name: 'repo-lead' }).waitFor()
  expect(await page.getByRole('tablist', { name: 'Open agents' }).getByRole('tab').count()).toBe(2)
  // The lead was on its chat view.
  await expect.poll(() => page.getByRole('region', { name: 'Main pane' }).getByRole('tab', { name: 'chat' }).getAttribute('aria-selected')).toBe('true')
  expect(await nav.getByRole('button', { name: 'Status' }).getAttribute('aria-pressed')).toBe('true')
  await page.screenshot({ path: path.join(shots, 'layout-after-restart.png') })
})
