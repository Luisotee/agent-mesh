import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole, MessageType } from '../src/gen/agentmesh/v1/types_pb'
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

/** What the page's clipboard writes did: the real window refuses them unless the main process lets the page write. */
const recordWrites = (page: App['page']) =>
  page.evaluate(() => {
    const w = window as unknown as { __writes: string[] }
    w.__writes = []
    const write = navigator.clipboard.writeText.bind(navigator.clipboard)
    navigator.clipboard.writeText = async (t: string) => {
      try {
        await write(t)
        w.__writes.push(`ok:${t}`)
      } catch (e) {
        w.__writes.push(`refused:${String(e)}`)
        throw e
      }
    }
  })
const writes = (page: App['page']) => page.evaluate(() => (window as unknown as { __writes: string[] }).__writes)

test('the copy button of a code block and the menus\' copies reach the clipboard, and say so', async () => {
  const { page } = app
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  const { run } = await host.client().startRun({ title: 'copy run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the lead', async () => (await host.client().listAgents({})).agents.length === 1)
  const lead = (await host.client().listAgents({})).agents[0]!
  await host.client(host.agentToken(lead.id)).spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })
  await until('the worker', async () => (await host.client().listAgents({})).agents.length === 2)
  const worker = (await host.client().listAgents({})).agents.find((a) => a.name.endsWith('fix'))!
  await host.client(host.agentToken(worker.id)).sendMessage({ to: 'parent', type: MessageType.DONE, body: 'Pair the phone:\n\n```\nmesh pair --name "Android phone"\n```\n' })
  await openFromSidebar(page, 'repo-lead')
  await recordWrites(page)

  // The code block's own button (Streamdown's): it writes the block's text, and its icon turns into a check for a moment.
  const block = page.locator('[data-streamdown="code-block"]').first()
  await block.waitFor()
  const copy = block.getByRole('button', { name: 'Copy Code' })
  const before = await copy.innerHTML()
  await copy.click()
  await expect.poll(async () => (await writes(page)).map((w) => w.trimEnd())).toEqual(['ok:mesh pair --name "Android phone"'])
  await expect.poll(() => copy.innerHTML()).not.toBe(before)
  await page.screenshot({ path: path.join(shots, 'copy-code-block.png') })

  // A menu's copy: the run's id, with a toast.
  const nav = page.getByRole('navigation', { name: 'Projects' })
  await nav.getByRole('listitem', { name: 'copy run' }).getByRole('button', { name: /copy run, lead repo-lead/ }).click({ button: 'right' })
  await page.getByRole('menuitem', { name: 'Copy run id' }).click()
  await page.getByRole('region', { name: 'Notices' }).getByText('Copied the run id').waitFor()
  expect((await writes(page)).at(-1)).toBe(`ok:${run!.id}`)
  await page.screenshot({ path: path.join(shots, 'copy-run-id.png') })
}, 120_000)
