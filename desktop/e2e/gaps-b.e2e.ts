import { execFileSync } from 'node:child_process'
import { appendFileSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { expect, test, beforeAll, afterAll } from 'vitest'
import { AgentRole } from '../src/gen/agentmesh/v1/types_pb'
import { launchApp, openFromSidebar, type App } from './electron'
import { buildBinaries, makeHost, until, type Host } from './harness'

let host: Host
let app: App
let bins: string

beforeAll(async () => {
  bins = mkdtempSync(path.join(os.tmpdir(), 'amd-bins-'))
  buildBinaries(bins)
  host = makeHost(bins)
  // A list of the ignored files a new worktree gets a copy of (git-ignored by the repo's own exclude file).
  appendFileSync(path.join(host.repo, '.git', 'info', 'exclude'), '.env\n')
  writeFileSync(path.join(host.repo, '.worktreeinclude'), '.env\n')
  writeFileSync(path.join(host.repo, '.env'), 'TOKEN=1\n')
  await host.start()
  app = await launchApp({ state: host.state, home: host.home })
})

afterAll(async () => {
  await app?.stop()
  await host?.dispose()
  if (bins) rmSync(bins, { recursive: true, force: true })
})

const git = (cwd: string, ...args: string[]) =>
  execFileSync('git', ['-C', cwd, '-c', 'user.name=t', '-c', 'user.email=t@t', ...args], { stdio: 'pipe', env: { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null' } }).toString()

test('a worktree is archived with a warning and restored, its files listed, the Tailscale switch asks first and the pairing refusal says why', async () => {
  const { page } = app
  await page.getByRole('status', { name: 'Host local, ready' }).waitFor()
  const { run } = await host.client().startRun({ title: 'archive run' })
  await host.client().spawnAgent({ runId: run!.id, repo: host.repo, slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: 'mid' })
  await until('the lead', async () => (await host.client().listAgents({})).agents.length === 1)
  const lead = (await host.client().listAgents({})).agents[0]!
  const { agent: worker } = await host.client(host.agentToken(lead.id)).spawnAgent({ repo: host.repo, slug: 'fix', role: AgentRole.WORKER, kind: 'claude', tier: 'mid' })
  const wt = worker!.worktree
  // The new worktree got the ignored file the repo listed.
  expect(readFileSync(path.join(wt, '.env'), 'utf8')).toBe('TOKEN=1\n')

  const nav = page.getByRole('navigation', { name: 'Projects' })
  await openFromSidebar(page, 'repo-fix')
  const side = page.getByRole('complementary', { name: 'Side panel' })
  await side.getByRole('region', { name: 'Agent tree' }).waitFor()

  // Uncommitted work is listed, and blocks the archive.
  writeFileSync(path.join(wt, 'notes.txt'), 'half done\n')
  await expect.poll(() => side.getByTestId('change-summary').innerText(), { timeout: 30_000 }).toContain('1 file uncommitted')
  expect(await side.getByRole('list', { name: 'Changed files' }).innerText()).toContain('notes.txt')
  await side.getByRole('button', { name: 'Archive' }).click()
  const dialog = page.getByRole('dialog', { name: 'Archive repo-fix?' })
  await expect.poll(() => dialog.getByRole('alert').first().innerText(), { timeout: 20_000 }).toContain('uncommitted changes')
  expect(await dialog.getByRole('button', { name: 'Archive', exact: true }).isDisabled()).toBe(true)
  await dialog.getByRole('button', { name: 'Cancel' }).click()

  // Committed, the commit is on no remote: the dialog asks to be sure, and the worktree goes, the branch stays.
  git(wt, 'add', 'notes.txt')
  git(wt, 'commit', '-q', '-m', 'notes')
  await side.getByRole('button', { name: 'Archive' }).click()
  await dialog.getByRole('checkbox').waitFor({ timeout: 20_000 })
  await dialog.getByRole('checkbox').check()
  await dialog.getByRole('button', { name: 'Archive', exact: true }).click()
  await side.getByRole('button', { name: 'Restore' }).waitFor({ timeout: 30_000 })
  expect(existsSync(wt)).toBe(false)
  expect(git(host.repo, 'branch', '--list', worker!.branch)).toContain(worker!.branch)
  // An archived agent leaves the sidebar (the open tab and the palette still reach it).
  await nav.locator('[data-sidebar-row="agent"][aria-label="repo-fix"]').waitFor({ state: 'detached' })

  // Restore: the worktree is made again from the branch, with the listed ignored file copied back.
  await side.getByRole('button', { name: 'Restore' }).click()
  await side.getByRole('button', { name: 'Archive' }).waitFor({ timeout: 40_000 })
  expect(readFileSync(path.join(wt, 'notes.txt'), 'utf8')).toBe('half done\n')
  expect(readFileSync(path.join(wt, '.env'), 'utf8')).toBe('TOKEN=1\n')

  // The Pair screen: no route, so the host refuses and says why; Tailscale is turned on only after a confirmation.
  await page.getByRole('button', { name: 'New', exact: true }).click()
  await page.getByRole('menuitem', { name: 'Pair a device' }).click()
  await page.getByRole('button', { name: 'Create pairing link' }).click()
  await page.getByRole('alert').filter({ hasText: 'no address a device could pair over' }).waitFor({ timeout: 20_000 })
  await page.getByRole('button', { name: 'Turn Tailscale on' }).click()
  const confirm = page.getByRole('dialog', { name: 'Turn Tailscale on?' })
  expect(await confirm.innerText()).toContain('Agents keep running')
  expect(existsSync(path.join(host.state, 'tailnet-on'))).toBe(false)
  await confirm.getByRole('button', { name: 'Turn on and restart' }).click()
  await page.getByText(/Restart meshd for it to take effect/).waitFor({ timeout: 20_000 })
  expect(existsSync(path.join(host.state, 'tailnet-on'))).toBe(true)
})
