import { mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { ActorClass } from '../src/gen/agentmesh/v1/actor_pb'
import { AgentRole, MessageType } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, type App, openFromSidebar } from './electron'
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

/** The user approves the plans of this repo: its workers' plans are then the user's to answer (and notify), not the lead's. */
async function userApprovesPlans() {
  await host.client().setSetting({ key: 'repos.user_approves_plans', value: { kind: { case: 'listValue', value: { values: [{ kind: { case: 'stringValue', value: host.repo } }] } } } })
}

test('the lead-agent view follows a real daemon live, and survives its restart', async () => {
  const { page } = app

  // Connected: the host's dot says ready, and the sidebar is empty.
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  await page.getByText(/No projects yet/).waitFor()

  // Start a lead from the UI: the empty sidebar's New lead, on the project, in a run of its own.
  await page.getByRole('button', { name: 'New lead', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Project').fill(host.repo)
  await dialog.getByLabel('Project').blur()
  await dialog.getByLabel('Run title').fill('e2e run')
  await dialog.getByLabel('Name').fill('lead')
  await dialog.getByRole('button', { name: 'Start workspace' }).click()
  await dialog.waitFor({ state: 'detached' })
  const nav = page.getByRole('navigation', { name: 'Projects' })
  const run = nav.getByRole('listitem', { name: 'e2e run' })
  const lead = run.getByRole('button', { name: 'e2e run, lead repo-lead' })
  await lead.waitFor({ timeout: 20_000 })
  await expect.poll(() => lead.innerText(), { timeout: 20_000 }).toContain('Idle, waiting for a message')

  // A worker the lead starts (with the lead's own token, as an agent would) joins the tree under it, live.
  const leadId = await agentId('repo-lead')
  const asLead = host.client(host.agentToken(leadId))
  const { agent: worker } = await asLead.spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })
  // It joins the run under its lead, folded until the lead is opened.
  await run.getByRole('button', { name: 'Show the workers of e2e run' }).click()
  const workerRow = run.getByRole('button', { name: 'repo-fix', exact: true })
  await workerRow.waitFor()
  await expect.poll(() => workerRow.innerText(), { timeout: 20_000 }).toContain('Idle')

  // Open the worker, message it from the UI, and the daemon has it, from the device.
  await workerRow.click()
  const panel = page.getByRole('article', { name: 'repo-fix' })
  await panel.getByRole('button', { name: 'Menu of repo-fix' }).click()
  await page.getByRole('menuitem', { name: 'Send a message…' }).click()
  const send = page.getByRole('dialog', { name: 'Message repo-fix' })
  await send.getByLabel('Message', { exact: true }).fill('please look at the typo')
  await send.getByRole('button', { name: 'Send' }).click()
  await send.waitFor({ state: 'detached' })
  const inbox = await host.client(host.agentToken(worker!.id)).readInbox({ peek: true, all: true })
  const sent = inbox.messages.find((m) => m.body === 'please look at the typo')
  expect(sent?.type).toBe(MessageType.TASK)
  expect(sent?.from?.name).toBeTruthy()

  // Stopping an agent from the UI ends its process; the tree shows it stopped, from the daemon's exit event.
  await panel.getByRole('button', { name: 'Menu of repo-fix' }).click()
  await page.getByRole('menuitem', { name: 'Stop…' }).click()
  await page.getByRole('button', { name: 'Stop repo-fix' }).click()
  await expect.poll(() => panel.getByTestId('agent-status').innerText(), { timeout: 20_000 }).toBe('Stopped')

  await page.screenshot({ path: path.join(shots, 'lead-agent-view.png') })

  // The daemon restarts: the app says so, reconnects by itself, and the tree is still there. An agent
  // started afterwards still arrives live.
  await host.stopDaemon()
  await page.getByRole('status').getByText(/unreachable/).waitFor({ timeout: 20_000 })
  await page.getByRole('status', { name: 'Host local, reconnecting' }).waitFor()
  await page.screenshot({ path: path.join(shots, 'host-down.png') })
  await host.start()
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor({ timeout: 30_000 })
  await lead.waitFor()
  await host.client().spawnAgent({ runId: await runId(), repo: host.repo, slug: 'after', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })
  await run.getByRole('button', { name: 'repo-after', exact: true }).waitFor()
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
  // The side panel takes a quarter of the width: a narrower terminal wraps the doorbells the lead gets and scrolls the
  // banner this test looks for off its screen, so it is hidden here (the side panel has its own test, `sidebar.e2e.ts`).
  await page.getByRole('button', { name: 'Hide the side panel' }).click().catch(() => {})
  await openFromSidebar(page, 'repo-lead')
  const panel = page.getByRole('article', { name: 'repo-lead' })
  await panel.getByRole('button', { name: 'Terminal' }).click()
  // The toggle shows the terminal in the chat's place; the hidden chat is not in the a11y tree.
  await expect.poll(() => panel.getByRole('button', { name: 'Terminal' }).getAttribute('aria-pressed')).toBe('true')
  await panel.getByRole('region', { name: 'Terminal' }).waitFor({ state: 'visible' })
  expect(await panel.getByRole('region', { name: 'Chat' }).count()).toBe(0)
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
  await userApprovesPlans()
  // A worker the lead started (so its parent is the lead, the plan's recipient).
  const asLead = host.client(host.agentToken(await agentId('repo-lead')))
  const { agent: worker } = await asLead.spawnAgent({ repo: host.repo, slug: 'planner', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })
  const asWorker = host.client(host.agentToken(worker!.id))
  await asWorker.sendMessage({ to: 'parent', type: MessageType.PLAN, body: '# Plan\n\n1. fix the typo\n2. add a test\n\nSee [the docs](https://example.com/docs).' })

  // The sidebar and its project's badge say it waits on the user; the plan is a card to read.
  const waiting = page.getByRole('region', { name: 'Waiting on you' })
  await waiting.getByRole('button', { name: /repo-planner sent a plan/ }).waitFor()
  await page.getByRole('navigation', { name: 'Projects' }).getByLabel(/waiting on you/).first().waitFor()
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

  // A question the lead puts to the user (`mesh send user`) is answered with free text, as an answer to the lead; a
  // worker's report is shown as Markdown.
  await asWorker.readInbox({}) // an agent reads its answer first: a report is refused while one is unread
  await asLead.sendMessage({ to: 'user', type: MessageType.QUESTION, body: 'Which branch should I base this on?' })
  await page.getByRole('region', { name: 'Waiting on you' }).getByRole('button', { name: /repo-lead asked a question/ }).click()
  const q = page.getByRole('form', { name: 'Answer the question' })
  await q.getByLabel('Your answer').fill('main')
  await q.getByRole('button', { name: 'Send answer' }).click()
  await expect.poll(async () => (await asLead.readInbox({ peek: true, all: true })).messages.map((m) => m.body)).toContain('main')
  await asWorker.sendMessage({ to: 'parent', type: MessageType.DONE, body: '## Result\n\n- **PR** opened\n- tests pass' })
  const report = page.getByRole('article', { name: 'Report from repo-planner' })
  await report.getByRole('heading', { name: 'Result' }).waitFor()
  expect(await report.getByRole('listitem').count()).toBe(2)
  await page.screenshot({ path: path.join(shots, 'messages.png') })
})

test('settings are generated from the host, set from the app, and change live when an agent sets them', async () => {
  const { page } = app
  await page.getByRole('button', { name: 'Settings', exact: true }).click()
  const panel = page.getByRole('region', { name: 'Settings' })
  await panel.getByLabel('Filter settings').fill('app.notify.sound')
  const row = panel.locator('[data-setting="app.notify.sound"]')
  const box = row.getByRole('checkbox', { name: 'Value of app.notify.sound' })
  await expect.poll(() => box.isChecked()).toBe(true)
  await page.screenshot({ path: path.join(shots, 'settings.png') })

  // Setting it in the app lands on the host, recorded with the device as who changed it.
  await box.click() // a controlled box: it changes when the host's event says so
  await expect.poll(() => box.isChecked()).toBe(false)
  await expect.poll(async () => (await host.client().listSettings({ prefix: 'app.notify.sound' })).settings[0]?.isSet).toBe(true)
  const set = (await host.client().listSettings({ prefix: 'app.notify.sound' })).settings[0]!
  expect(set.value?.kind).toEqual({ case: 'boolValue', value: false })
  expect(set.updatedBy?.class).toBe(ActorClass.DEVICE)

  // The host refuses a value outside the schema, and the app says why.
  await panel.getByLabel('Filter settings').fill('quota_stop')
  const quota = panel.getByRole('spinbutton', { name: 'Value of quota_stop' })
  await quota.fill('150')
  await quota.blur()
  await expect.poll(() => panel.getByRole('alert').innerText()).toContain('whole number 0 to 100')

  // Someone else (an agent, another device) sets it: the open screen follows, no reload.
  await host.client().setSetting({ key: 'quota_stop', value: { kind: { case: 'numberValue', value: 80 } } })
  await expect.poll(() => quota.inputValue()).toBe('80')
  await panel.getByRole('button', { name: 'Reset quota_stop' }).click()
  await expect.poll(() => quota.inputValue()).toBe('90')
  await expect.poll(async () => (await host.client().listSettings({ prefix: 'quota_stop' })).settings[0]?.isSet).toBe(false)

  // And back: the sound is on again, as the default.
  await panel.getByLabel('Filter settings').fill('app.notify.sound')
  await row.getByRole('button', { name: 'Reset app.notify.sound' }).click()
  await expect.poll(() => box.isChecked()).toBe(true)
  await page.getByRole('button', { name: 'Settings', exact: true }).click() // back to the agents
})

test('notifications follow the host\'s settings, history stays quiet, and the tray is told what waits', async () => {
  const { page } = app
  const log = () => readFileSync(app.shellLog, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l) as Record<string, unknown>)
  const notices = () => log().filter((l) => l.type === 'notify').map((l) => `${l.title}`)
  await userApprovesPlans()
  const asLead = host.client(host.agentToken(await agentId('repo-lead')))
  const { agent: noisy } = await asLead.spawnAgent({ repo: host.repo, slug: 'noisy', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })
  const asNoisy = host.client(host.agentToken(noisy!.id))

  await asNoisy.sendMessage({ to: 'parent', type: MessageType.PLAN, body: '# Plan\n\n- one thing' })
  await expect.poll(notices, { timeout: 20_000 }).toContain('repo-noisy sent a plan')
  const entry = log().find((l) => l.type === 'notify' && l.title === 'repo-noisy sent a plan')!
  expect(entry).toMatchObject({ tab: 'chat', sound: true, body: 'Plan one thing' })
  await expect.poll(() => log().filter((l) => l.type === 'waiting').at(-1)?.count, { timeout: 20_000 }).toBe(1)

  // A page reload replays nothing into notifications.
  const before = notices().length
  await page.reload()
  await page.getByRole('region', { name: 'Waiting on you' }).waitFor({ timeout: 20_000 })
  await new Promise((r) => setTimeout(r, 1000))
  expect(notices()).toHaveLength(before)

  // Take plans off the list of what notifies (through the host's settings): the next plan is silent, the tray still counts.
  await host.client().setSetting({ key: 'app.notify.events', value: { kind: { case: 'listValue', value: { values: [{ kind: { case: 'stringValue', value: 'input' } }] } } } })
  await asNoisy.readInbox({})
  await asNoisy.sendMessage({ to: 'parent', type: MessageType.QUESTION, body: 'Which branch?' })
  await expect.poll(() => log().filter((l) => l.type === 'waiting').at(-1)?.count, { timeout: 20_000 }).toBe(2)
  expect(notices()).toHaveLength(before)
  await host.client().resetSetting({ key: 'app.notify.events' })
})

test('the theme is a host setting: each of the six redraws the app, live, and an agent can change it', async () => {
  const { page } = app
  const themeOf = () => page.evaluate(() => ({ name: document.documentElement.dataset.theme, bg: getComputedStyle(document.body).backgroundColor, density: document.documentElement.dataset.density }))
  expect((await themeOf()).name).toBe('Dracula')
  await page.getByRole('button', { name: 'Settings', exact: true }).click()
  const panel = page.getByRole('region', { name: 'Settings' })
  await panel.getByLabel('Filter settings').fill('app.theme')
  const cards = panel.getByRole('radiogroup', { name: 'Value of app.theme' })
  const seen = new Map<string, string>()
  for (const [name, file, bg] of [
    ['Alucard', 'alucard', 'rgb(255, 251, 235)'],
    ['Catppuccin Mocha', 'catppuccin-mocha', 'rgb(30, 30, 46)'],
    ['Catppuccin Latte', 'catppuccin-latte', 'rgb(239, 241, 245)'],
    ['Tokyo Night', 'tokyo-night', 'rgb(26, 27, 38)'],
    ['Nord', 'nord', 'rgb(46, 52, 64)'],
    ['Dracula', 'dracula', 'rgb(40, 42, 54)'],
  ] as const) {
    await cards.getByRole('radio', { name, exact: true }).click()
    await expect.poll(async () => (await themeOf()).name).toBe(name)
    expect((await themeOf()).bg).toBe(bg)
    seen.set(name, (await themeOf()).density ?? '')
    await page.screenshot({ path: path.join(shots, `theme-${file}.png`) })
  }
  expect(seen.get('Nord')).toBe('comfortable')
  expect(seen.get('Dracula')).toBe('compact')
  // The host has it, recorded as set by the device.
  await expect.poll(async () => (await host.client().listSettings({ prefix: 'app.theme' })).settings[0]?.isSet).toBe(true)

  // An agent (or the CLI) sets it and the open app follows, no reload.
  await host.client().setSetting({ key: 'app.theme', value: { kind: { case: 'stringValue', value: 'Nord' } } })
  await expect.poll(async () => (await themeOf()).name).toBe('Nord')
  // A name this build lacks leaves the default look.
  await host.client().setSetting({ key: 'app.theme', value: { kind: { case: 'stringValue', value: 'Solarized' } } })
  await expect.poll(async () => (await themeOf()).name).toBe('Dracula')
  await host.client().resetSetting({ key: 'app.theme' })
  await page.getByRole('button', { name: 'Settings', exact: true }).click() // back to the agents
})

test('a repo is kept from free models by a switch in its project settings, opened from the settings screen, and the host then refuses OpenCode there', async () => {
  const { page } = app
  await page.getByRole('button', { name: 'Settings', exact: true }).click()
  const panel = page.getByRole('region', { name: 'Settings' })
  await panel.getByLabel('Filter settings').fill('free models')
  await panel.getByRole('button', { name: /^Project settings: repo$/ }).click()
  const dialog = page.getByRole('dialog', { name: /repo settings/ })
  const box = dialog.getByRole('checkbox', { name: 'Free models allowed' })
  await expect.poll(() => box.isChecked()).toBe(true)
  await page.screenshot({ path: path.join(shots, 'free-models.png') })
  await box.click()
  await expect.poll(() => box.isChecked()).toBe(false)
  const listed = async () => (await host.client().listSettings({ prefix: 'repos.no_free_models' })).settings[0]
  await expect.poll(async () => (await listed())?.isSet).toBe(true)

  // The host enforces it: an OpenCode agent there is refused, naming the setting.
  const asLead = host.client(host.agentToken(await agentId('repo-lead')))
  await expect(asLead.spawnAgent({ repo: host.repo, slug: 'free', role: AgentRole.WORKER, kind: 'opencode', tier: 'light' })).rejects.toThrow(/repos\.no_free_models/)

  // Switched back on, and the list is empty again.
  await box.click()
  await expect.poll(() => box.isChecked()).toBe(true)
  await expect.poll(async () => JSON.stringify((await listed())?.value?.kind)).toContain('"values":[]')
  await dialog.getByRole('button', { name: 'Done' }).click()
  await page.getByRole('button', { name: 'Settings', exact: true }).click() // back to the agents
})

test('the usage and cost screen asks the host for what mesh usage and mesh cost print', async () => {
  const { page } = app
  await page.getByRole('button', { name: 'Hosts' }).click()
  await page.getByRole('menuitem', { name: 'Usage and cost' }).click()
  const panel = page.getByRole('region', { name: 'Usage' })
  await panel.getByRole('heading', { name: /Usage and cost of local/ }).waitFor()
  // A throwaway host has no Claude account (the screen says so), and the fake agy reports two quota rows, one nearly spent.
  await panel.getByText(/Reserve: the host starts nothing new on a quota once 90% of it is used/).waitFor({ timeout: 30_000 })
  await panel.getByText(/No reading/).waitFor()
  await expect.poll(() => panel.getByRole('row', { name: /Neon/ }).innerText()).toContain('5% left')
  await expect.poll(() => panel.getByRole('alert').innerText()).toContain('agy Gemini 4 Neon Neon')
  // The same figures the host computes for the CLI: tokens first, the dollars beside them.
  await panel.getByTestId('cost-total').waitFor({ timeout: 30_000 })
  const cli = await host.client().getCost({ since: '7d' })
  await expect.poll(() => panel.getByTestId('cost-total').innerText()).toContain('tokens')
  await expect.poll(() => panel.getByTestId('cost-usd').innerText()).toContain(`≈ $${cli.usd.toFixed(2)}`)
  await page.screenshot({ path: path.join(shots, 'usage.png') })

  // A run is a scope of its own, named by its title.
  await panel.getByRole('combobox', { name: 'Cost of' }).selectOption({ label: 'e2e run' })
  await expect.poll(() => panel.getByTestId('cost-scope').innerText(), { timeout: 30_000 }).toContain('run ')
  await page.getByRole('button', { name: 'Hosts' }).click()
  await page.getByRole('menuitem', { name: 'Usage and cost' }).click() // back to the agents
})
