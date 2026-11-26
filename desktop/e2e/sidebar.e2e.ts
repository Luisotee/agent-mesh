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

  // Back to runs: the tree again.
  await group.getByRole('button', { name: 'Runs' }).click()
  await nav.getByRole('region', { name: 'group run' }).waitFor()
})
