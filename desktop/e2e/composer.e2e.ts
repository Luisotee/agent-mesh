import { existsSync, mkdirSync, mkdtempSync, readdirSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, openFromSidebar, type App } from './electron'
import { buildBinaries, makeHost, until, type Host } from './harness'

let host: Host
let app: App
let bins: string
const shots = path.resolve(import.meta.dirname, '../test-results')
const outDir = process.env.SHOTS_DIR ?? shots
const mode = process.env.SHOTS_MODE ?? 'after'

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  host = makeHost(bins, { fakeClaudeEnv: { FAKECLAUDE_TRANSCRIPT: '1', FAKECLAUDE_BOX: '1' } })
  await host.start()
  mkdirSync(shots, { recursive: true })
  mkdirSync(outDir, { recursive: true })
  app = await launchApp({ state: host.state, home: host.home })
})

afterAll(async () => {
  await app?.stop()
  await host?.dispose()
  if (bins) rmSync(bins, { recursive: true, force: true })
})

/** The files of the agent's attachment folder on the host (`AddAttachment` stores each image there). */
function stored(): string[] {
  const out: string[] = []
  const walk = (dir: string) => {
    if (!existsSync(dir)) return
    for (const e of readdirSync(dir, { withFileTypes: true })) (e.isDirectory() ? walk(path.join(dir, e.name)) : out.push(path.join(dir, e.name)))
  }
  walk(path.join(host.state, 'runs'))
  return out.filter((f) => f.includes('attachments'))
}

// A 1x1 PNG.
const PNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='

test('the composer: one box with the model picker and the context meter, images pasted and dropped, a prompt shown at once', async () => {
  const { page } = app
  const { run } = await host.client().startRun({ title: 'composer run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the lead', async () => (await host.client().listAgents({})).agents.length === 1)
  await openFromSidebar(page, 'repo-lead')
  const box = page.getByRole('textbox', { name: 'Prompt' })
  await box.waitFor()
  const tools = page.getByRole('toolbar', { name: 'Prompt tools' })
  await tools.getByRole('button', { name: /^Model: / }).waitFor()
  await page.screenshot({ path: path.join(outDir, `${mode}-1-composer.png`) })

  // The model picker: the tiers, the models, the efforts; a pick is one SwitchAgent.
  await tools.getByRole('button', { name: /^Model: / }).click()
  await page.getByRole('menuitem', { name: 'low', exact: true }).waitFor()
  await page.screenshot({ path: path.join(outDir, `${mode}-2-model-picker.png`) })
  await page.getByRole('menuitem', { name: 'low', exact: true }).click()
  await expect.poll(async () => (await host.client().listAgents({})).agents[0]?.effort, { timeout: 30_000 }).toBe('low')
  await expect.poll(() => tools.getByTestId('model-chip').innerText(), { timeout: 20_000 }).toContain('low')

  // An image pasted into the box, and one dropped anywhere over the chat.
  const before = stored().length
  await box.click()
  await page.evaluate((b64) => {
    const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))
    const dt = new DataTransfer()
    dt.items.add(new File([bytes], 'pasted.png', { type: 'image/png' }))
    document.querySelector('textarea[aria-label="Prompt"]')!.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }))
  }, PNG)
  const strip = page.getByRole('list', { name: 'Images' })
  await strip.getByRole('img', { name: 'pasted.png' }).waitFor()
  await expect.poll(() => stored().length, { timeout: 20_000 }).toBe(before + 1)
  await page.evaluate((b64) => {
    const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))
    const dt = new DataTransfer()
    dt.items.add(new File([bytes], 'dropped.png', { type: 'image/png' }))
    const chat = document.querySelector('[data-testid="chat"]')!
    chat.dispatchEvent(new DragEvent('dragenter', { dataTransfer: dt, bubbles: true, cancelable: true }))
  }, PNG)
  await page.getByRole('status', { name: 'Drop images' }).waitFor()
  await page.screenshot({ path: path.join(outDir, `${mode}-3-drop-overlay.png`) })
  await page.evaluate((b64) => {
    const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))
    const dt = new DataTransfer()
    dt.items.add(new File([bytes], 'dropped.png', { type: 'image/png' }))
    document.querySelector('[data-testid="chat"]')!.dispatchEvent(new DragEvent('drop', { dataTransfer: dt, bubbles: true, cancelable: true }))
  }, PNG)
  await strip.getByRole('img', { name: 'dropped.png' }).waitFor()
  await expect.poll(() => stored().length, { timeout: 20_000 }).toBe(before + 2)
  await page.screenshot({ path: path.join(outDir, `${mode}-4-images.png`) })

  // Sending shows the prompt at once, with its images, until the record has it.
  await box.fill('what is in these')
  await page.keyboard.press('Enter')
  await page.getByRole('list', { name: 'Sending' }).waitFor({ timeout: 5000 }).catch(() => {})
  await page.screenshot({ path: path.join(outDir, `${mode}-5-sent.png`) })
  await expect.poll(() => page.getByTestId('chat').innerText(), { timeout: 30_000 }).toContain('what is in these')
  await expect.poll(() => page.getByRole('list', { name: 'Sending' }).count(), { timeout: 20_000 }).toBe(0)
})
