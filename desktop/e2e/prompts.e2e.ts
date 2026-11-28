import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole, AgentState } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, type App } from './electron'
import { buildBinaries, makeHost, until, type Host } from './harness'

let host: Host
let app: App
let bins: string
const shots = path.resolve(import.meta.dirname, '../test-results')

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  host = makeHost(bins, { fakeClaudeEnv: { FAKECLAUDE_TRANSCRIPT: '1', FAKECLAUDE_BOX: '1' } })
  await host.start()
  mkdirSync(shots, { recursive: true })
  app = await launchApp({ state: host.state, home: host.home })
})

afterAll(async () => {
  await app?.stop()
  await host?.dispose()
  if (bins) rmSync(bins, { recursive: true, force: true })
})

// What an agent waits on that only the user answers shows as a card in its chat, and answering it gives the agent
// what the terminal's own dialog would (the fake claude acts the hook out as the real one: `/question`, `/exitplan`, `/perm`).
test('a question, a plan and a push are answered from the chat', async () => {
  const { page } = app
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  const { run } = await host.client().startRun({ title: 'prompts' })
  const { agent } = await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  const id = agent!.id
  await until('the lead to be idle', async () => (await host.client().getAgent({ agent: id })).agent?.state === AgentState.IDLE)

  const dock = page.getByRole('region', { name: 'Live dock' })
  await dock.getByRole('listitem', { name: 'repo-lead in the dock' }).getByRole('button', { name: /Idle/ }).click()
  const panel = page.getByRole('article', { name: 'repo-lead' })
  await panel.getByRole('region', { name: 'Chat' }).waitFor({ state: 'visible' })
  const ask = (text: string) => host.client().promptAgent({ agent: id, text })

  // A question: two questions, one with previews; a single choice, a multi-select and the text written under Other.
  await ask('/question')
  const card = panel.getByRole('article', { name: '2 questions from repo-lead' })
  await card.waitFor()
  await card.getByLabel('Preview').getByText('RED swatch').waitFor()
  await card.getByRole('radio', { name: /Blue/ }).hover()
  await card.getByLabel('Preview').getByText('BLUE swatch').waitFor()
  await card.getByRole('radio', { name: /Blue/ }).click()
  await card.getByRole('checkbox', { name: /Cheese/ }).click()
  await card.getByRole('checkbox', { name: /Ham/ }).click()
  await card.getByLabel('Other answer to Which toppings do you want?').fill('extra jam')
  await page.screenshot({ path: path.join(shots, 'prompt-question.png') })
  await card.getByRole('button', { name: 'Send answers' }).click()
  const list = panel.getByRole('list', { name: 'Conversation' })
  await expect.poll(() => list.innerText(), { timeout: 20_000 }).toContain('→ Cheese, Ham, extra jam')
  expect(await list.innerText()).toContain('→ Blue')
  expect(await card.count()).toBe(0)
  await page.screenshot({ path: path.join(shots, 'prompt-question-answered.png') })

  // A plan: approved to go on with accepted edits.
  await until('the lead to be idle', async () => (await host.client().getAgent({ agent: id })).agent?.state === AgentState.IDLE)
  await ask('/exitplan')
  const plan = panel.getByRole('article', { name: 'Plan from repo-lead' })
  await plan.getByText('Create hello.txt').waitFor()
  await plan.getByLabel('After approving').selectOption('acceptEdits')
  await page.screenshot({ path: path.join(shots, 'prompt-plan.png') })
  await plan.getByRole('button', { name: 'Approve' }).click()
  await expect.poll(() => plan.count(), { timeout: 20_000 }).toBe(0)
  await until('the lead to be idle', async () => (await host.client().getAgent({ agent: id })).agent?.state === AgentState.IDLE)

  // A push is the user's, never a parent's: denied with a reason the agent reads.
  await ask('/perm Bash git push origin main')
  const tool = panel.getByRole('article', { name: 'Permission from repo-lead' })
  await tool.getByText('git push origin main').waitFor()
  await page.screenshot({ path: path.join(shots, 'prompt-permission.png') })
  await tool.getByRole('button', { name: 'Deny…' }).click()
  await page.getByLabel('Why repo-lead may not').fill('open a pull request instead')
  await page.getByRole('button', { name: 'Deny', exact: true }).click()
  await expect.poll(() => tool.count(), { timeout: 20_000 }).toBe(0)
  await until('the lead to be idle', async () => (await host.client().getAgent({ agent: id })).agent?.state === AgentState.IDLE)
})
