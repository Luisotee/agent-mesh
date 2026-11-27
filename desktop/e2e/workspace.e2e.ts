import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { ActorClass } from '../src/gen/agentmesh/v1/actor_pb'
import { TranscriptKind } from '../src/gen/agentmesh/v1/chat_pb'
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
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  const dock = page.getByRole('region', { name: 'Live dock' })
  await dock.getByText('No live leads').waitFor()

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
  await panel.getByRole('button', { name: 'Terminal' }).click()
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

test('the command palette across the host, and shortcuts rebound from the settings or by another device', async () => {
  const { page } = app
  const tabs = page.getByRole('tablist', { name: 'Open agents' })
  const palette = page.getByRole('combobox', { name: 'Search commands' })
  const dialog = page.getByRole('dialog', { name: 'Command palette' })

  // The default chord opens it; typing filters; Enter goes there as a tab.
  await page.keyboard.press('Control+Shift+P')
  await palette.waitFor()
  await palette.fill('repo-fix')
  await page.screenshot({ path: path.join(shots, 'palette.png') })
  await page.getByRole('option', { name: /Go to repo-fix/ }).waitFor()
  await page.keyboard.press('Enter')
  await tabs.getByRole('tab', { name: /repo-fix/, selected: true }).waitFor()
  await dialog.waitFor({ state: 'detached' })

  // Tabs cycle on Ctrl+Tab, and Escape closes the palette.
  await page.keyboard.press('Control+Tab')
  await tabs.getByRole('tab', { name: /repo-lead/, selected: true }).waitFor()
  await page.keyboard.press('Control+Shift+Tab')
  await tabs.getByRole('tab', { name: /repo-fix/, selected: true }).waitFor()
  await page.keyboard.press('Control+Shift+P')
  await palette.waitFor()
  await page.keyboard.press('Escape')
  await dialog.waitFor({ state: 'detached' })

  // An action of the registry from the palette: ring... retire the worker (confirmed first), and the host says so.
  await page.keyboard.press('Control+Shift+P')
  await palette.fill('retire repo-fix')
  await page.keyboard.press('Enter')
  await page.getByText(/its conversation is kept/i).waitFor()
  await page.getByRole('button', { name: 'Retire repo-fix' }).click()
  await expect.poll(async () => (await host.client().listAgents({})).agents.find((a) => a.name === 'repo-fix')?.state, { timeout: 20_000 }).toBe(AgentState.RETIRED)
  await dialog.waitFor({ state: 'detached' })

  // Rebinding from the settings screen: the palette's own "Keyboard shortcuts…" opens it filtered.
  await page.keyboard.press('Control+Shift+P')
  await palette.fill('keyboard shortcuts')
  await page.keyboard.press('Enter')
  const settings = page.getByRole('region', { name: 'Settings' })
  await settings.waitFor()
  const row = settings.locator('[data-setting="app.keys.palette"]')
  await row.waitFor()
  await page.screenshot({ path: path.join(shots, 'shortcuts.png') })
  await row.getByRole('button', { name: 'Record a shortcut for app.keys.palette' }).click()
  await page.keyboard.press('Control+Alt+K')
  await expect.poll(async () => (await host.client().listSettings({ prefix: 'app.keys.palette' })).settings[0]?.isSet, { timeout: 10_000 }).toBe(true)
  const stored = (await host.client().listSettings({ prefix: 'app.keys.palette' })).settings[0]!
  expect(stored.value?.kind).toEqual({ case: 'stringValue', value: 'Mod+Alt+K' })
  expect(stored.updatedBy?.class).toBe(ActorClass.DEVICE)

  // The old chord does nothing now, and the new one opens it.
  await page.keyboard.press('Control+Shift+P')
  await new Promise((r) => setTimeout(r, 300))
  expect(await palette.count()).toBe(0)
  await page.keyboard.press('Control+Alt+K')
  await palette.waitFor()
  await page.keyboard.press('Escape')

  // Another device (here the CLI's client) rebinds it, and the open app follows with no reload; a reset gives the default back.
  await host.client().setSetting({ key: 'app.keys.palette', value: { kind: { case: 'stringValue', value: 'Mod+Shift+O' } } })
  await expect.poll(async () => {
    await page.keyboard.press('Control+Shift+O')
    return palette.count().then((n) => n > 0)
  }, { timeout: 10_000 }).toBe(true)
  await page.keyboard.press('Escape')
  // The host refuses a value that is not a chord, and says why.
  await expect(host.client().setSetting({ key: 'app.keys.palette', value: { kind: { case: 'stringValue', value: 'P' } } })).rejects.toThrow(/key chord/)
  await host.client().resetSetting({ key: 'app.keys.palette' })
  await expect.poll(async () => {
    await page.keyboard.press('Control+Shift+P')
    return palette.count().then((n) => n > 0)
  }, { timeout: 10_000 }).toBe(true)
  await page.keyboard.press('Escape')
})

test('the chat view: the conversation as blocks, a prompt typed from the composer, switching with the terminal', async () => {
  const { page } = app
  const tabs = page.getByRole('tablist', { name: 'Open agents' })
  await tabs.getByRole('tab', { name: /repo-lead/ }).click()
  const panel = page.getByRole('article', { name: 'repo-lead' })
  if (await panel.getByRole('region', { name: 'Terminal' }).isVisible()) await panel.getByRole('button', { name: 'Terminal' }).click()
  await panel.getByRole('region', { name: 'Chat' }).waitFor({ state: 'visible' })
  await panel.getByText('Nothing said yet.').waitFor()

  // A prompt from the composer reaches the agent's CLI, and its turn shows up in the conversation.
  const box = panel.getByRole('textbox', { name: 'Prompt' })
  // A send is queued when the agent can't take it yet (still starting) and typed at its next idle: the box empties
  // at once. Sent again only if the host refused outright.
  const send = () =>
    expect
      .poll(async () => {
        await box.press('Enter')
        await page.waitForTimeout(400)
        return box.inputValue()
      }, { timeout: 20_000 })
      .toBe('')
  await box.fill('fix the typo')
  await send()
  const list = panel.getByRole('list', { name: 'Conversation' })
  await expect.poll(() => list.innerText(), { timeout: 20_000 }).toContain('fix the typo')
  await expect.poll(() => list.innerText(), { timeout: 20_000 }).toContain('Done: fix the typo')
  await panel.getByText('Ran 1 tool: Bash').waitFor()
  await expect.poll(() => box.inputValue()).toBe('')
  await page.screenshot({ path: path.join(shots, 'chat.png') })

  // It is the same process: the terminal, from the toggle shortcut, shows the same turn.
  await page.keyboard.press('Control+Shift+J')
  await panel.getByRole('region', { name: 'Terminal' }).waitFor({ state: 'visible' })
  await expect.poll(() => panel.locator('.xterm-rows').innerText(), { timeout: 20_000 }).toContain('done: fix the typo')
  await page.keyboard.press('Control+Shift+J')
  await panel.getByRole('region', { name: 'Chat' }).waitFor({ state: 'visible' })

  // Several lines are one prompt; the host refuses to type into a draft someone left in the terminal.
  await box.fill('first line')
  await box.press('Shift+Enter')
  await box.type('second line')
  await send()
  await expect.poll(() => list.innerText(), { timeout: 20_000 }).toContain('Done: first line')
  expect(await list.innerText()).toContain('second line')

  // The conversation was written by the CLI, not by us: ReadTranscript says the same through the API.
  const lead = (await host.client().listAgents({})).agents.find((a) => a.name === 'repo-lead')!
  const read = await host.client().readTranscript({ agent: lead.id, offset: -1n, backward: true })
  expect(read.supported).toBe(true)
  expect(read.entries.filter((e) => e.kind === TranscriptKind.USER).map((e) => e.text)).toEqual(['fix the typo', 'first line\nsecond line'])

  // Mid-turn a send is queued, never refused: it shows waiting under the conversation, Up takes the newest back to
  // edit, and what stays queued is typed once the turn ends.
  await page.keyboard.press('Control+Shift+J')
  await panel.getByTestId('terminal').click()
  await page.keyboard.type('/hold')
  await page.keyboard.press('Enter')
  await page.keyboard.press('Control+Shift+J')
  await expect.poll(() => panel.getByTestId('chat-status').innerText(), { timeout: 20_000 }).toContain('Working')
  await box.fill('after the hold')
  await box.press('Enter')
  const queue = panel.getByRole('region', { name: 'Queued prompts' })
  await queue.getByText('after the hold').waitFor()
  await queue.getByText(/Sent when it finishes its turn/).waitFor()
  await box.fill('second thoughts')
  await box.press('Enter')
  await queue.getByText('second thoughts').waitFor()
  await page.screenshot({ path: path.join(shots, 'chat-queued.png') })
  await box.press('ArrowUp')
  await expect.poll(() => box.inputValue()).toBe('second thoughts')
  await queue.getByText('second thoughts').waitFor({ state: 'detached' })
  await box.fill('')
  // The held turn ends (Enter in its terminal): the queued prompt goes in.
  await page.keyboard.press('Control+Shift+J')
  await panel.getByTestId('terminal').click()
  await page.keyboard.press('Enter')
  await page.keyboard.press('Control+Shift+J')
  await expect.poll(() => list.innerText(), { timeout: 20_000 }).toContain('Done: after the hold')
  await queue.waitFor({ state: 'detached' })
  expect(await list.innerText()).not.toContain('second thoughts')
})
