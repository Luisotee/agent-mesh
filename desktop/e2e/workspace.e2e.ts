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

async function agentId(name: string): Promise<string> {
  let id = ''
  await until(`agent ${name}`, async () => {
    const { agents } = await host.client().listAgents({})
    id = agents.find((a) => a.name === name)?.id ?? ''
    return !!id
  })
  return id
}

test('the live dock: a tile per agent, a new worker pops in without taking focus, a click opens it as a tab', async () => {
  const { page } = app
  await page.getByRole('button', { name: 'Host local, ready' }).waitFor()
  const dock = page.getByRole('region', { name: 'Live dock' })
  await dock.getByText('No live agents').waitFor()

  const { run } = await host.client().startRun({ title: 'dock run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  const leadTile = dock.getByRole('listitem', { name: 'repo-lead in the dock' })
  await leadTile.waitFor()
  await expect.poll(() => leadTile.innerText(), { timeout: 20_000 }).toContain('Idle')

  // A click opens the lead as a tab.
  await leadTile.getByRole('button', { name: /Idle/ }).click()
  const tabs = page.getByRole('tablist', { name: 'Open agents' })
  await tabs.getByRole('tab', { name: /repo-lead/ }).waitFor()
  await page.getByRole('article', { name: 'repo-lead' }).waitFor()

  // A worker the lead starts pops into the dock marked new. The lead stays open, and no tab is added for it.
  const asLead = host.client(host.agentToken(await agentId('repo-lead')))
  await asLead.spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })
  const fixTile = dock.getByRole('listitem', { name: 'repo-fix in the dock' })
  await fixTile.waitFor()
  await fixTile.getByText('new', { exact: true }).waitFor()
  expect(await page.getByRole('article', { name: 'repo-lead' }).count()).toBe(1)
  expect(await tabs.getByRole('tab').count()).toBe(1)
  await page.screenshot({ path: path.join(shots, 'dock-new-worker.png') })

  // Clicking it opens it as a tab and clears the mark.
  await fixTile.getByRole('button', { name: /repo-fix/ }).first().click()
  await tabs.getByRole('tab', { name: /repo-fix/, selected: true }).waitFor()
  await page.getByRole('article', { name: 'repo-fix' }).waitFor()
  expect(await dock.getByText('new', { exact: true }).count()).toBe(0)

  // What the worker runs shows live in its tile: /hold leaves a tool running (an ephemeral frame, no state event).
  const panel = page.getByRole('article', { name: 'repo-fix' })
  await panel.getByRole('tab', { name: 'terminal' }).click()
  await expect.poll(() => panel.getByTestId('terminal-status').innerText(), { timeout: 20_000 }).toBe('Live')
  await panel.getByTestId('terminal').click()
  await page.keyboard.type('/hold')
  await page.keyboard.press('Enter')
  await expect.poll(() => fixTile.innerText(), { timeout: 20_000 }).toContain('Working: Bash')
  await page.screenshot({ path: path.join(shots, 'dock-working.png') })
  await page.keyboard.press('Enter') // the held turn ends
  await expect.poll(() => fixTile.innerText(), { timeout: 20_000 }).toContain('Idle')

  // Closing the tab leaves the agent in the dock.
  await tabs.getByRole('button', { name: 'Close repo-fix' }).click()
  await tabs.getByRole('tab', { name: /repo-fix/ }).waitFor({ state: 'detached' })
  await fixTile.waitFor()
})
