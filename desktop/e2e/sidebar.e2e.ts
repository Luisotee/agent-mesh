import { existsSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, type App } from './electron'
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

test('the sidebar shows the project, its run as the lead and the worker under it; the dock and the panel follow the open agent', async () => {
  const { page } = app
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  const { run } = await host.client().startRun({ title: 'group run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the lead', async () => (await host.client().listAgents({})).agents.length === 1)
  const { agents } = await host.client().listAgents({})
  await host.client(host.agentToken(agents[0]!.id)).spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })

  // What the app asks for to tell "ready to review": a device must name the run, and the host answers a row per change worker.
  const prs = await host.client().listPullRequests({ runId: run!.id })
  expect(prs.agents.map((a) => a.agentName)).toContain('repo-fix')

  const nav = page.getByRole('navigation', { name: 'Projects' })
  const project = nav.getByRole('listitem', { name: 'repo' })
  const lead = project.getByRole('button', { name: 'group run, lead repo-lead' })
  await lead.waitFor()
  // The worker is folded under its lead until the chevron opens it.
  expect(await project.getByRole('button', { name: 'repo-fix', exact: true }).count()).toBe(0)
  await project.getByRole('button', { name: 'Show the workers of group run' }).click()
  const fix = project.getByRole('button', { name: 'repo-fix', exact: true })
  await fix.waitFor()
  // The worker's CLI and model as chips.
  await expect.poll(() => fix.innerText()).toContain('repo')
  await page.screenshot({ path: path.join(shots, 'sidebar-projects.png') })

  // The side panel shows the open lead's tree and its run total (nothing spent by a fake CLI).
  await lead.click()
  const side = page.getByRole('complementary', { name: 'Side panel' })
  const tree = side.getByRole('list', { name: 'Agent tree' })
  // The idle worker rests behind a count (the tree lists live agents); a worker still starting is in it.
  const resting = side.getByRole('button', { name: /^\d+ resting$/ })
  await resting.or(tree.getByRole('button', { name: /repo-fix/ })).first().waitFor()
  if (await resting.isVisible()) await resting.click()
  await tree.getByRole('button', { name: /repo-fix/ }).waitFor()
  await expect.poll(() => side.getByRole('region', { name: 'Run total' }).innerText(), { timeout: 20_000 }).toContain('$0.00')
  expect(await side.getByRole('region', { name: 'Pull requests' }).innerText()).toContain('None yet')
  // The dock shows this run only.
  await expect.poll(() => page.getByRole('region', { name: 'Live dock' }).innerText()).toContain('in this run')
  await page.screenshot({ path: path.join(shots, 'side-panel.png') })
  await page.getByRole('button', { name: 'Hide the side panel' }).click()
  await side.waitFor({ state: 'detached' })

  // A right-click on the project opens its menu.
  await project.getByRole('button', { name: 'repo', exact: true }).click({ button: 'right' })
  await page.getByRole('menuitem', { name: 'Project settings…' }).click()
  await page.getByRole('dialog', { name: /repo settings/ }).getByRole('button', { name: 'Done' }).click()

  // Watch all: a tile per live agent, the fake CLI's conversation under each.
  await page.getByRole('button', { name: 'Watch all' }).click()
  const grid = page.getByRole('region', { name: 'Watch all' })
  await grid.getByRole('listitem', { name: 'repo-fix in the grid' }).waitFor()
  expect(await grid.getByRole('listitem', { name: /in the grid/ }).count()).toBe(2)
  await page.screenshot({ path: path.join(shots, 'watch-all.png') })
  await grid.getByRole('button', { name: 'Open repo-fix' }).click()
  await page.getByRole('article', { name: 'repo-fix' }).waitFor()
  await expect.poll(() => fix.getAttribute('aria-current')).toBe('true')
})

test('a new workspace starts a lead in a worktree of a project from the dialog, and holding a PR needs a PR the host lists', async () => {
  const { page } = app
  await page.getByRole('button', { name: /^New in / }).click()
  await page.getByRole('menuitem', { name: 'New lead…' }).click()
  const dialog = page.getByRole('dialog')
  // The host lists the repos it has seen (the throwaway repo, used by the earlier test's agents), with its branches.
  await dialog.getByLabel('Project').fill(host.repo)
  await dialog.getByLabel('Project').blur()
  await dialog.getByText(/Default branch: main|Default branch: the repo's HEAD/).waitFor({ timeout: 20_000 })
  await dialog.getByLabel('Start from', { exact: true }).selectOption('branch')
  await dialog.getByLabel('Branch').selectOption('main')
  await dialog.getByLabel('Run title').fill('from the dialog')
  await dialog.getByLabel('Name').fill('dialog-lead')
  await page.screenshot({ path: path.join(shots, 'workspace-dialog.png') })
  await dialog.getByRole('button', { name: 'Start workspace' }).click()
  // The dialog closes and the new agent's terminal opens.
  await dialog.waitFor({ state: 'detached' })
  await page.getByRole('article', { name: 'repo-dialog-lead' }).waitFor({ timeout: 20_000 })
  const created = (await host.client().listAgents({})).agents.find((a) => a.name === 'repo-dialog-lead')!
  expect(created.branch).toContain('dialog-lead')

  // A host with no such PR refuses a hold (nothing would carry it to the root).
  await expect(host.client().holdPullRequest({ url: 'https://github.com/o/r/pull/1' })).rejects.toThrow(/no pull request/)
})

test('a stopped run is deleted from the sidebar: its row, its folder and the host\'s record go, and an open tab of it closes', async () => {
  const { page } = app
  const { run } = await host.client().startRun({ title: 'to delete' })
  const { agent } = await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'gone', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the agent', async () => (await host.client().listAgents({})).agents.some((a) => a.id === agent!.id))
  const nav = page.getByRole('navigation', { name: 'Projects' })
  const project = nav.getByRole('listitem', { name: 'repo' })
  await project.getByRole('button', { name: 'to delete, lead repo-gone' }).click()
  await page.getByRole('article', { name: 'repo-gone' }).waitFor({ timeout: 20_000 })

  // A run with a process can't be deleted: the entry is there and disabled.
  await project.getByRole('button', { name: 'Menu of to delete' }).click()
  await expect.poll(() => page.getByRole('menuitem', { name: 'Delete…' }).getAttribute('aria-disabled')).toBe('true')
  await page.keyboard.press('Escape')
  await expect(host.client().deleteRun({ runId: run!.id })).rejects.toThrow(/stop the run first/)

  await host.client().stopRun({ runId: run!.id })
  const earlier = project.getByRole('button', { name: /^Earlier/ })
  await earlier.waitFor()
  if ((await earlier.getAttribute('aria-expanded')) !== 'true') await earlier.click()
  // The row leaves "Running" as it arrives under "Earlier": wait for the one.
  await expect.poll(() => project.getByRole('button', { name: 'Menu of to delete' }).count()).toBe(1)
  await project.getByRole('button', { name: 'Menu of to delete' }).click()
  await page.getByRole('menuitem', { name: 'Delete…' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete the run' }).click()
  await project.getByRole('button', { name: 'Menu of to delete' }).waitFor({ state: 'detached', timeout: 20_000 })
  expect(await page.getByRole('article', { name: 'repo-gone' }).count()).toBe(0)
  await page.screenshot({ path: path.join(shots, 'delete-run.png') })
  await until('the host forgot the run', async () => !(await host.client().listRuns({ all: true })).runs.some((r) => r.id === run!.id))
  expect(existsSync(path.join(host.state, 'runs', run!.id))).toBe(false)
})
