import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, type App } from './electron'
import { buildBinaries, makeHost, type Host } from './harness'

let host: Host
let app: App
let bins: string
const shots = path.resolve(import.meta.dirname, '../test-results')

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  host = makeHost(bins, { fakeClaudeEnv: { FAKECLAUDE_TRANSCRIPT: '1', FAKECLAUDE_BOX: '1', FAKECLAUDE_SUGGESTION: 'run the tests again' } })
  await host.start()
  mkdirSync(shots, { recursive: true })
  app = await launchApp({ state: host.state, home: host.home })
})

afterAll(async () => {
  await app?.stop()
  await host?.dispose()
  if (bins) rmSync(bins, { recursive: true, force: true })
})

test('the composer\'s / popup lists the agent\'s commands, completes one, shows what a command printed and takes the user to a dialog', async () => {
  const { page } = app
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  const { run } = await host.client().startRun({ title: 'composer run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  const dock = page.getByRole('region', { name: 'Live dock' })
  const tile = dock.getByRole('listitem', { name: 'repo-lead in the dock' })
  await tile.waitFor()
  await expect.poll(() => tile.innerText(), { timeout: 20_000 }).toContain('Idle')
  await tile.getByRole('button', { name: /Idle/ }).click()
  const panel = page.getByRole('article', { name: 'repo-lead' })
  if (await panel.getByRole('region', { name: 'Terminal' }).isVisible()) await panel.getByRole('button', { name: 'Terminal' }).click()
  await panel.getByRole('region', { name: 'Chat' }).waitFor({ state: 'visible' })
  const box = panel.getByRole('textbox', { name: 'Prompt' })

  // A slash opens the popup with the host's commands (the CLI's own list and the terminal's), each with its description.
  await page.screenshot({ path: path.join(shots, 'composer-before.png') })
  await box.click()
  await box.fill('/')
  const popup = page.getByRole('listbox', { name: 'Commands' })
  await popup.waitFor({ timeout: 20_000 })
  await expect.poll(() => popup.innerText()).toContain('/compact')
  expect(await popup.innerText()).toContain('Free up context by summarizing')
  expect(await popup.innerText()).toContain('/resume')
  await page.screenshot({ path: path.join(shots, 'composer-popup.png') })

  // Typing filters; Tab takes the highlighted command and the popup closes.
  await box.fill('/com')
  await expect.poll(async () => (await popup.getByRole('option').count()) === 1).toBe(true)
  await page.screenshot({ path: path.join(shots, 'composer-filtered.png') })
  await box.press('Tab')
  await expect.poll(() => box.inputValue()).toBe('/compact ')
  expect(await popup.count()).toBe(0)

  // A command the CLI prints for shows its output in the chat.
  await box.fill('/context')
  await box.press('Enter') // the popup's: the name is whole, so this one sends
  const list = panel.getByRole('list', { name: 'Conversation' })
  await expect.poll(() => list.innerText(), { timeout: 20_000 }).toContain('/context')
  await expect.poll(() => list.innerText(), { timeout: 20_000 }).toContain('Context 12k/200k tokens')
  await page.screenshot({ path: path.join(shots, 'composer-output.png') })

  // After the turn Claude draws its suggested next prompt in its box: the composer shows it as grey text, Tab takes it,
  // and it goes once the box has anything in it.
  await box.fill('')
  await expect.poll(() => box.getAttribute('placeholder'), { timeout: 30_000 }).toBe('run the tests again')
  await page.screenshot({ path: path.join(shots, 'composer-suggestion.png') })
  await box.press('Tab')
  await expect.poll(() => box.inputValue()).toBe('run the tests again')
  await box.fill('')

  // A command that opens a dialog in the terminal takes the user there.
  await box.fill('/resume')
  await box.press('Enter')
  await panel.getByRole('region', { name: 'Terminal' }).waitFor({ state: 'visible', timeout: 20_000 })
})
