import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole, MessageType } from '../src/gen/agentmesh/v1/types_pb'
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

test('the lead-agent view follows a real daemon live, and survives its restart', async () => {
  const { page } = app

  // Connected: the host's dot says ready, and the sidebar is empty.
  await page.getByRole('button', { name: 'Host local, ready' }).waitFor()
  await page.getByText('No runs yet').waitFor()

  // Start a run from the UI, then its lead.
  await page.getByRole('button', { name: 'New run' }).click()
  await page.getByLabel('Title').fill('e2e run')
  await page.getByRole('button', { name: 'Start run' }).click()
  const run = page.getByRole('region', { name: 'e2e run' })
  await run.waitFor()

  await run.getByRole('button', { name: /Add an agent to e2e run/ }).click()
  await page.getByLabel('Repository').fill(host.repo)
  await page.getByLabel('Slug').fill('lead')
  await page.getByRole('button', { name: 'Start agent' }).click()
  const lead = run.getByRole('button', { name: /repo-lead/ })
  await lead.waitFor()
  await expect.poll(() => lead.innerText(), { timeout: 20_000 }).toContain('Idle, waiting for a message')

  // A worker the lead starts (with the lead's own token, as an agent would) joins the tree under it, live.
  const leadId = await agentId('repo-lead')
  const asLead = host.client(host.agentToken(leadId))
  const { agent: worker } = await asLead.spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude' })
  const workerRow = run.getByRole('listitem').filter({ has: page.getByRole('button', { name: /repo-lead/ }) }).getByRole('list').getByRole('button', { name: /repo-fix/ })
  await workerRow.waitFor()
  await expect.poll(() => workerRow.innerText(), { timeout: 20_000 }).toContain('Idle')

  // Open the worker, message it from the UI, and the daemon has it, from the device.
  await workerRow.click()
  const panel = page.getByRole('article', { name: 'repo-fix' })
  await panel.getByLabel('Message', { exact: true }).fill('please look at the typo')
  await panel.getByRole('button', { name: 'Send' }).click()
  await panel.getByText('Sent.').waitFor()
  const inbox = await host.client(host.agentToken(worker!.id)).readInbox({ peek: true, all: true })
  const sent = inbox.messages.find((m) => m.body === 'please look at the typo')
  expect(sent?.type).toBe(MessageType.TASK)
  expect(sent?.from?.name).toBeTruthy()

  // Stopping an agent from the UI ends its process; the tree shows it stopped, from the daemon's exit event.
  await panel.getByRole('button', { name: 'Stop agent' }).click()
  await panel.getByRole('button', { name: 'Stop repo-fix' }).click()
  await expect.poll(() => panel.getByTestId('agent-status').innerText(), { timeout: 20_000 }).toBe('Stopped')

  await page.screenshot({ path: path.join(shots, 'lead-agent-view.png') })

  // The daemon restarts: the app says so, reconnects by itself, and the tree is still there. An agent
  // started afterwards still arrives live.
  await host.stopDaemon()
  await page.getByRole('status').getByText(/unreachable/).waitFor({ timeout: 20_000 })
  await page.getByRole('button', { name: 'Host local, reconnecting' }).waitFor()
  await page.screenshot({ path: path.join(shots, 'host-down.png') })
  await host.start()
  await page.getByRole('button', { name: 'Host local, ready' }).waitFor({ timeout: 30_000 })
  await run.getByRole('button', { name: /repo-lead/ }).waitFor()
  await host.client().spawnAgent({ runId: await runId(), repo: host.repo, slug: 'after', role: AgentRole.WORKER, kind: 'claude' })
  await run.getByRole('button', { name: /repo-after/ }).waitFor()
})

async function runId(): Promise<string> {
  const { runs } = await host.client().listRuns({})
  const r = runs.find((x) => x.title === 'e2e run')
  if (!r) throw new Error('no run')
  return r.id
}

async function agentId(name: string): Promise<string> {
  let id = ''
  await until(`agent ${name}`, async () => {
    const { agents } = await host.client().listAgents({})
    id = agents.find((a) => a.name === name)?.id ?? ''
    return !!id
  })
  return id
}

test('a terminal streams the agent, takes typing, and carries on across a daemon restart', async () => {
  const { page } = app
  await page.getByRole('button', { name: /repo-lead/ }).click()
  const panel = page.getByRole('article', { name: 'repo-lead' })
  await panel.getByRole('tab', { name: 'terminal' }).click()
  // The selected tab controls a visible panel of the same name; the hidden one is not in the a11y tree.
  expect(await panel.getByRole('tab', { name: 'terminal' }).getAttribute('aria-selected')).toBe('true')
  await panel.getByRole('tabpanel', { name: 'terminal' }).waitFor({ state: 'visible' })
  expect(await panel.getByRole('tabpanel', { name: 'overview' }).count()).toBe(0)
  const status = panel.getByTestId('terminal-status')
  const screen = panel.locator('.xterm-rows')
  await expect.poll(() => status.innerText(), { timeout: 20_000 }).toBe('Live')
  await expect.poll(() => screen.innerText()).toContain('fake claude')

  // Typing reaches the program, and its answer comes back.
  await panel.getByTestId('terminal').click()
  await page.keyboard.type('hello')
  await page.keyboard.press('Enter')
  await expect.poll(() => screen.innerText(), { timeout: 20_000 }).toContain('done: hello')

  // A permission prompt: the sidebar says it needs you, and answering in the terminal clears it.
  await page.keyboard.type('/ask')
  await page.keyboard.press('Enter')
  await expect.poll(() => panel.getByTestId('agent-status').innerText(), { timeout: 20_000 }).toContain('Needs you')
  await page.keyboard.type('y')
  await page.keyboard.press('Enter')
  await expect.poll(() => panel.getByTestId('agent-status').innerText(), { timeout: 20_000 }).toContain('Idle')
  await page.screenshot({ path: path.join(shots, 'terminal.png') })

  // The daemon restarts; the terminal host and its program keep running, the stream comes back by offset
  // (the screen is not wiped and not repeated), and typing still works.
  await host.stopDaemon()
  await expect.poll(() => status.innerText(), { timeout: 20_000 }).toMatch(/^Reconnecting/)
  await host.start()
  await expect.poll(() => status.innerText(), { timeout: 30_000 }).toBe('Live')
  // xterm draws what it was written a moment after the stream says live: wait for the screen, then check it
  // wasn't repeated.
  await expect.poll(() => screen.innerText(), { timeout: 10_000 }).toContain('done: hello')
  expect((await screen.innerText()).match(/done: hello/g)).toHaveLength(1)
  await panel.getByTestId('terminal').click()
  await page.keyboard.type('again')
  await page.keyboard.press('Enter')
  await expect.poll(() => screen.innerText(), { timeout: 20_000 }).toContain('done: again')
})

test('plans, questions and reports: waiting on the user, answered from the app, and still there after a reload', async () => {
  const { page } = app
  // A worker the lead started (so its parent is the lead, the plan's recipient).
  const asLead = host.client(host.agentToken(await agentId('repo-lead')))
  const { agent: worker } = await asLead.spawnAgent({ repo: host.repo, slug: 'planner', role: AgentRole.WORKER, kind: 'claude' })
  const asWorker = host.client(host.agentToken(worker!.id))
  await asWorker.sendMessage({ to: 'parent', type: MessageType.PLAN, body: '# Plan\n\n1. fix the typo\n2. add a test\n\nSee [the docs](https://example.com/docs).' })

  // The sidebar and the host's badge say it waits on the user; the plan is a card to read.
  const waiting = page.getByRole('region', { name: 'Waiting on you' })
  await waiting.getByRole('button', { name: /repo-planner sent a plan/ }).waitFor()
  await page.getByRole('button', { name: /Host local/ }).locator('xpath=.//span[contains(@class,"bg-state-needs")]').waitFor()
  await waiting.getByRole('button', { name: /repo-planner sent a plan/ }).click()
  const panel = page.getByRole('article', { name: 'repo-planner' })
  const plan = panel.getByRole('article', { name: 'Plan from repo-planner' })
  await expect.poll(() => plan.innerText()).toContain('fix the typo')
  await page.screenshot({ path: path.join(shots, 'plan.png') })

  // A reload loses the page's state; ListMessages brings the plan back, still waiting.
  await page.reload()
  await page.getByRole('region', { name: 'Waiting on you' }).getByRole('button', { name: /repo-planner sent a plan/ }).waitFor({ timeout: 20_000 })

  // Approving sends an answer to the worker, with the device as its actor, and closes the item.
  await page.getByRole('region', { name: 'Waiting on you' }).getByRole('button', { name: /repo-planner sent a plan/ }).click()
  await page.getByRole('article', { name: 'Plan from repo-planner' }).getByRole('button', { name: 'Approve' }).click()
  await expect.poll(async () => (await asWorker.readInbox({ peek: true, all: true })).messages.map((m) => `${MessageType[m.type]}:${m.body}:${m.from?.name}`)).toContainEqual(expect.stringMatching(/^ANSWER:approved:/))
  await page.getByRole('region', { name: 'Waiting on you' }).waitFor({ state: 'detached', timeout: 20_000 })

  // A question is answered with free text; a report is shown as Markdown.
  await asWorker.sendMessage({ to: 'parent', type: MessageType.QUESTION, body: 'Which branch should I base this on?' })
  await page.getByRole('region', { name: 'Waiting on you' }).getByRole('button', { name: /repo-planner asked a question/ }).click()
  const q = page.getByRole('form', { name: 'Answer the question' })
  await q.getByLabel('Your answer').fill('main')
  await q.getByRole('button', { name: 'Send answer' }).click()
  await expect.poll(async () => (await asWorker.readInbox({ peek: true, all: true })).messages.map((m) => m.body)).toContain('main')
  await asWorker.sendMessage({ to: 'parent', type: MessageType.DONE, body: '## Result\n\n- **PR** opened\n- tests pass' })
  const report = page.getByRole('article', { name: 'Report from repo-planner' })
  await report.getByRole('heading', { name: 'Result' }).waitFor()
  expect(await report.getByRole('listitem').count()).toBe(2)
  await page.screenshot({ path: path.join(shots, 'messages.png') })
})
