import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import type { Page } from 'playwright-core'
import { afterAll, beforeAll, expect, test } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, type App } from './electron'
import { buildBinaries, freePort, makeHost, startRelay, until, type Host, type Relay } from './harness'

// Pair one app with a host over a pinned TLS address and then through a relay: the host's own app makes the
// links on its Pair a device screen, a second app (with no fixed host) adds the host by pasting each.

let bins: string
let relay: Relay
let host: Host
let owner: App
let shell: string
let listen: string
const others: App[] = []
const shots = path.resolve(import.meta.dirname, '../test-results')

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  shell = mkdtempSync(path.join(os.tmpdir(), 'amd-r-'))
  relay = await startRelay(bins, shell)
  listen = `127.0.0.1:${await freePort()}`
  host = makeHost(bins, { listen, relay, hostName: 'e2e-home' })
  await host.start()
  mkdirSync(shots, { recursive: true })
  owner = await launchApp({ state: host.state, home: host.home })
}, 180_000)

afterAll(async () => {
  for (const a of others) await a.stop()
  await owner?.stop()
  await host?.dispose()
  await relay?.stop()
  if (shell) rmSync(shell, { recursive: true, force: true })
  if (bins) rmSync(bins, { recursive: true, force: true })
})

/** The Pair a device screen of the host's own app: make a link (with this direct address) and read it. */
async function makeLink(page: Page, directUrl: string, deviceName: string): Promise<string> {
  const screen = page.getByRole('region', { name: 'Pair a device' })
  if (!(await screen.isVisible())) await page.getByRole('button', { name: 'Pair a device' }).click()
  await screen.waitFor()
  // The real host runs without --tailscale: the host's own app asks TailnetStatus and says the node is off.
  await screen.getByRole('region', { name: 'Tailscale' }).getByText(/--tailscale/).waitFor()
  const again = screen.getByRole('button', { name: /^(Make another link|Pair another device)$/ })
  if (await again.isVisible().catch(() => false)) await again.click()
  await screen.getByLabel('Name of the device').fill(deviceName)
  await screen.getByLabel('Address the device reaches this host at').fill(directUrl)
  await screen.getByRole('button', { name: 'Create pairing link' }).click()
  await screen.getByRole('img', { name: 'Pairing QR code' }).waitFor()
  return screen.getByLabel('Pairing link').inputValue()
}

/** A second app with a host list of its own, and the host added to it by `link`. */
async function pairedApp(link: string, name: string): Promise<App> {
  const home = mkdtempSync(path.join(os.tmpdir(), 'amd-h2-'))
  const app = await launchApp({ home })
  others.push(app)
  const { page } = app
  // No host yet: the first-run screen looks for a daemon here (there is none, in a test's home), and the form is behind this.
  await page.getByRole('button', { name: 'Connect another way' }).click()
  await page.getByLabel('How to reach it').selectOption('pair')
  await page.getByLabel('Pairing link').fill(link)
  await page.getByLabel("This device's name").fill(name)
  await page.getByRole('button', { name: 'Add host' }).click()
  return app
}

async function openLeadTerminal(app: App, status: string) {
  const { page } = app
  await page.getByRole('button', { name: 'Host e2e-home, ready' }).waitFor({ timeout: 30_000 })
  // At its prompt first: a terminal attached while the fake is still starting can lose its first line (TODO.md).
  const lead = page.getByRole('navigation', { name: 'Runs' }).getByRole('button', { name: /repo-lead/ })
  await lead.filter({ hasText: 'Idle' }).waitFor({ timeout: 30_000 })
  await lead.click()
  const panel = page.getByRole('article', { name: 'repo-lead' })
  await panel.getByRole('tab', { name: 'terminal' }).click()
  const st = panel.getByTestId('terminal-status')
  await expect.poll(() => st.innerText(), { timeout: 30_000, message: status }).toBe('Live')
  await expect.poll(() => panel.locator('.xterm-rows').innerText(), { timeout: 20_000 }).toContain('fake claude')
}

test('a second app pairs by the link, over the direct address, and watches an agent live', async () => {
  // A run with a lead the second app will see, started by the host's own client.
  const c = host.client()
  const { run } = await c.startRun({ title: 'paired run' })
  await c.spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude' })

  const link = await makeLink(owner.page, `https://${listen}`, 'Second desktop')
  await owner.page.screenshot({ path: path.join(shots, 'pair.png') })
  expect(link).toMatch(/^agentmesh:\/\/pair\?/)
  expect(link).toContain('rtcp=') // the relay is in every link of a host that has one

  const second = await pairedApp(link, 'Second desktop')
  await second.page.getByRole('region', { name: 'paired run' }).waitFor({ timeout: 30_000 })
  await openLeadTerminal(second, 'direct')
  await second.page.screenshot({ path: path.join(shots, 'paired-direct.png') })

  // The host's own app sees the device arrive, and the link says so.
  await owner.page.getByRole('status').filter({ hasText: 'Paired: Second desktop' }).waitFor()
  const row = owner.page.getByRole('list', { name: 'Paired devices' }).getByRole('listitem').filter({ hasText: 'Second desktop' })
  await row.waitFor()

  // Revoking it from the host: the second app's token stops working and it says so.
  await row.getByRole('button', { name: 'Revoke access of Second desktop' }).click()
  await row.getByRole('button', { name: 'Revoke Second desktop', exact: true }).click()
  await row.waitFor({ state: 'detached' })
  await second.page.getByRole('button', { name: 'Host e2e-home, stopped' }).waitFor({ timeout: 30_000 })
  await second.page.getByRole('status').filter({ hasText: /didn't accept the app's token/ }).waitFor()
})

test('a link whose direct address is dead falls back to the relay, and a used link is refused', async () => {
  const link = await makeLink(owner.page, `https://127.0.0.1:${await freePort()}`, 'Through the relay')
  const third = await pairedApp(link, 'Through the relay')
  await openLeadTerminal(third, 'relay')
  await third.page.screenshot({ path: path.join(shots, 'paired-relay.png') })

  // The token in a link works once: the same link in another app is refused by the host, and nothing is saved.
  const home = mkdtempSync(path.join(os.tmpdir(), 'amd-h3-'))
  const fourth = await launchApp({ home })
  others.push(fourth)
  await fourth.page.getByRole('button', { name: 'Connect another way' }).click()
  await fourth.page.getByLabel('How to reach it').selectOption('pair')
  await fourth.page.getByLabel('Pairing link').fill(link)
  await fourth.page.getByRole('button', { name: 'Add host' }).click()
  await fourth.page.getByRole('alert').filter({ hasText: /refused the pairing/ }).waitFor({ timeout: 30_000 })
  await until('no host to have been saved', async () => (await fourth.page.getByRole('button', { name: /^Host / }).count()) === 0, 2000)
})

test('a link naming a certificate the host doesn\'t have is refused before anything is sent', async () => {
  const link = (await makeLink(owner.page, `https://${listen}`, 'Wrong pin')).replace(/fp=sha256%3A[0-9a-f]{64}|fp=sha256:[0-9a-f]{64}/i, `fp=sha256:${'cd'.repeat(32)}`)
  expect(link).toContain('cdcdcd')
  const home = mkdtempSync(path.join(os.tmpdir(), 'amd-h4-'))
  const app = await launchApp({ home })
  others.push(app)
  await app.page.getByRole('button', { name: 'Connect another way' }).click()
  await app.page.getByLabel('How to reach it').selectOption('pair')
  await app.page.getByLabel('Pairing link').fill(link)
  await app.page.getByRole('button', { name: 'Add host' }).click()
  await app.page.getByRole('alert').filter({ hasText: /isn't the one it was paired with|couldn't reach the host to pair/ }).waitFor({ timeout: 30_000 })
})

test('an app opened by a pairing link shows it filled in, pairs only when told, and the host matches the device to the link exactly', async () => {
  // The link is made with one name; the device that uses it calls itself something else, and another device is
  // paired meanwhile by hand: only the pairing id can tell which one used the link.
  const link = await makeLink(owner.page, `https://${listen}`, 'Named on the host')
  const home = mkdtempSync(path.join(os.tmpdir(), 'amd-h5-'))
  const opened = await launchApp({ home, args: [link] })
  others.push(opened)
  const { page } = opened
  await expect.poll(() => page.getByLabel('Pairing link').inputValue(), { timeout: 30_000 }).toBe(link)
  await page.getByRole('note').filter({ hasText: 'opened by a pairing link' }).waitFor()
  await page.screenshot({ path: path.join(shots, 'opened-by-link.png') })
  // Nothing was redeemed: the host's link is still on show.
  await owner.page.getByRole('img', { name: 'Pairing QR code' }).waitFor()

  await page.getByLabel("This device's name").fill('Desktop opened by link')
  await page.getByRole('button', { name: 'Add host' }).click()
  await page.getByRole('button', { name: /^Host e2e-home/ }).waitFor({ timeout: 30_000 })
  await owner.page.getByRole('status').filter({ hasText: 'Paired: Desktop opened by link' }).waitFor({ timeout: 30_000 })

  // A link clicked while the app runs reaches it through the single-instance lock, and opens the dialog filled in.
  const second = await makeLink(owner.page, `https://${listen}`, 'Clicked while running')
  await opened.openLink(second)
  const dialog = page.getByRole('dialog')
  await expect.poll(() => dialog.getByLabel('Pairing link').inputValue(), { timeout: 30_000 }).toBe(second)
  await page.keyboard.press('Escape')
  await dialog.waitFor({ state: 'detached' })
  // Still not redeemed.
  await owner.page.getByRole('img', { name: 'Pairing QR code' }).waitFor()
})
