import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
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

test('the sidebar groups the agents by run, project and status bucket', async () => {
  const { page } = app
  await page.getByRole('button', { name: 'Host local, ready' }).waitFor()
  const { run } = await host.client().startRun({ title: 'group run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the lead', async () => (await host.client().listAgents({})).agents.length === 1)
  const { agents } = await host.client().listAgents({})
  await host.client(host.agentToken(agents[0]!.id)).spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })

  // What the app asks for to tell "ready to review": a device must name the run, and the host answers a row per change worker.
  const prs = await host.client().listPullRequests({ runId: run!.id })
  expect(prs.agents.map((a) => a.agentName)).toContain('repo-fix')

  const nav = page.getByRole('navigation', { name: 'Runs' })
  await nav.getByRole('button', { name: /repo-fix/ }).waitFor()
  const group = nav.getByRole('group', { name: 'Group the agents by' })
  await group.getByRole('button', { name: 'Projects' }).click()
  const project = nav.getByRole('region', { name: 'repo' })
  await project.getByRole('button', { name: /repo-lead/ }).waitFor()
  await project.getByRole('button', { name: /repo-fix/ }).waitFor()

  // Both stop at their prompt, which the host reports as idle: one bucket, with the run named on each row.
  await group.getByRole('button', { name: 'Status' }).click()
  const idle = nav.getByRole('region', { name: 'Idle' })
  await expect.poll(() => idle.getByRole('button').count(), { timeout: 20_000 }).toBe(2)
  expect(await idle.innerText()).toContain('repo · group run')
  await page.screenshot({ path: path.join(shots, 'sidebar-status.png') })

  // The side panel shows the open lead's run: its cost (nothing spent by a fake CLI), its place and the host's quota.
  await nav.getByRole('button', { name: 'Runs', exact: true }).click()
  await nav.getByRole('button', { name: /repo-lead/ }).click()
  const side = page.getByRole('complementary', { name: 'Side panel' })
  await side.getByRole('region', { name: 'Run' }).waitFor()
  await expect.poll(() => side.getByTestId('cost-total').innerText(), { timeout: 20_000 }).toBe('$0.00')
  expect(await side.getByRole('region', { name: 'Pull requests' }).innerText()).toContain('None yet')
  await page.screenshot({ path: path.join(shots, 'side-panel.png') })
  await page.getByRole('button', { name: 'Hide the side panel' }).click()
  await side.waitFor({ state: 'detached' })

  // Watch all: a tile per live agent, the fake CLI's conversation under each.
  await page.getByRole('button', { name: 'Watch all' }).click()
  const grid = page.getByRole('region', { name: 'Watch all' })
  await grid.getByRole('listitem', { name: 'repo-fix in the grid' }).waitFor()
  expect(await grid.getByRole('listitem', { name: /in the grid/ }).count()).toBe(2)
  await page.screenshot({ path: path.join(shots, 'watch-all.png') })
  await grid.getByRole('button', { name: 'Open repo-fix' }).click()
  await page.getByRole('article', { name: 'repo-fix' }).waitFor()

  // Back to runs: the tree again.
  await group.getByRole('button', { name: 'Runs' }).click()
  await nav.getByRole('region', { name: 'group run' }).waitFor()
})
