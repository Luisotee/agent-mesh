import { mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, openFromSidebar, type App } from './electron'
import { buildBinaries, makeHost, until, type Host } from './harness'

let host: Host
let app: App
let bins: string

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  host = makeHost(bins)
  await host.start()
  app = await launchApp({ state: host.state, home: host.home })
})

afterAll(async () => {
  await app?.stop()
  await host?.dispose()
  if (bins) rmSync(bins, { recursive: true, force: true })
})

test('a run is renamed in place, a held permission prompt is allowed from the app, and an agent opens a view in it', async () => {
  const { page } = app
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  const { run } = await host.client().startRun({ title: 'first title' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the lead', async () => (await host.client().listAgents({})).agents.length === 1)
  const lead = (await host.client().listAgents({})).agents[0]!
  const asLead = host.client(host.agentToken(lead.id))
  const { agent: worker } = await asLead.spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })

  // Rename: the run's menu (or F2) edits the title in place; the host's event updates the sidebar (and so would another device's).
  const nav = page.getByRole('navigation', { name: 'Projects' })
  await nav.getByRole('button', { name: 'Menu of first title' }).click()
  await page.getByRole('menuitem', { name: /Rename/ }).click()
  await page.getByLabel('Run title').fill('the shop checkout')
  await page.getByLabel('Run title').press('Enter')
  await nav.getByRole('listitem', { name: 'the shop checkout' }).waitFor()
  expect((await host.client().listRuns({})).runs.find((r) => r.id === run!.id)?.title).toBe('the shop checkout')

  // A worker at a permission prompt the host holds for its lead: Allow and Deny are in "Waiting on you".
  // Type the fake CLI's `/perm` line in the worker's terminal, as a person would.
  await openFromSidebar(page, 'repo-fix')
  const panel = page.getByRole('article', { name: 'repo-fix' })
  await page.getByRole('group', { name: 'View' }).getByRole('button', { name: 'CLI' }).click()
  await expect.poll(() => panel.getByTestId('terminal-status').innerText(), { timeout: 20_000 }).toBe('Live')
  await panel.getByTestId('terminal').click()
  await page.keyboard.type('/perm Bash rm -rf build')
  await page.keyboard.press('Enter')
  const waiting = page.getByRole('region', { name: 'Waiting on you' })
  const prompt = waiting.getByLabel('repo-fix asks to use Bash')
  await prompt.waitFor({ timeout: 20_000 })
  expect(await prompt.innerText()).toContain('rm -rf build')
  await prompt.getByRole('button', { name: 'Allow' }).click()
  await prompt.waitFor({ state: 'detached', timeout: 20_000 })
  expect((await host.client().listPermissions({})).permissions).toEqual([])

  // An agent asks the app to show a view: a toast the user opens (it never switches their view by itself).
  const told = await asLead.openView({ view: 'agent', target: 'repo-fix' })
  expect(told.delivered).toBeGreaterThan(0)
  const toast = page.getByRole('status').filter({ hasText: 'wants to show you' })
  await toast.waitFor({ timeout: 20_000 })
  expect(await toast.innerText()).toContain("repo-fix's chat")
  await toast.getByRole('button', { name: 'Open' }).click()
  await page.getByRole('article', { name: 'repo-fix' }).waitFor({ timeout: 20_000 })
})
