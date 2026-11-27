import { useState } from 'react'
import { setSetting } from '@/actions'
import { LEVEL_LABEL, hasLevels, levelOf, listOf, nameEntries, withLevel, type Level } from '@/autonomy'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Select } from '@/components/ui/input'
import { useApp, useAppState, useHostState } from '@/context'
import { repoName, sameRepo } from '@/derive'
import type { HostRuntime } from '@/model'
import { jsonOf } from '@/prefs'

const FREE_KEY = 'repos.no_free_models'
const say = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/**
 * A project's settings, from its sidebar menu: how much its leads do on their own (the host's `repos.user_merges` and
 * `repos.user_approves_plans` lists, for this repo), whether its code may go to free models (`repos.no_free_models`),
 * and its pin. New runs in the project start from this level; a run started with a level of its own keeps that one.
 */
export function ProjectSettingsDialog() {
  const app = useApp()
  const at = useAppState((s) => s.projectSettings)
  const host = useAppState((s) => s.hosts.find((h) => h.info.id === at?.hostId))
  if (!at || !host) return null
  return (
    <Dialog open onOpenChange={(o) => !o && app.openProjectSettings(undefined)}>
      <DialogContent title={`${repoName(at.repo)} settings`} description={at.repo}>
        <ProjectSettings host={host} repo={at.repo} />
      </DialogContent>
    </Dialog>
  )
}

function ProjectSettings({ host, repo }: { host: HostRuntime; repo: string }) {
  const app = useApp()
  const settings = useHostState(host, (s) => s.settings)
  const pinned = useAppState((s) => (s.pins[host.info.id] ?? []).includes(repo))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const level = levelOf(settings, repo)
  const byName = nameEntries(settings, repo)
  const free = jsonOf(settings[FREE_KEY])
  const kept = Array.isArray(free) && free.some((l) => typeof l === 'string' && sameRepo(l, repo))
  async function choose(next: Level) {
    setError('')
    setBusy(true)
    // Two settings move together: when the second write fails the first is put back, so the repo is never left in neither list.
    const done: Array<{ key: string; value: string[] }> = []
    try {
      for (const w of withLevel(settings, repo, next)) {
        await setSetting(host.client, w.key, w.value)
        done.push({ key: w.key, value: settings[w.key] ? listOf(settings, w.key) : [] })
      }
    } catch (e) {
      for (const back of done.reverse()) await setSetting(host.client, back.key, back.value).catch(() => {})
      setError(say(e))
    } finally {
      setBusy(false)
    }
  }
  async function setFree(mayUse: boolean) {
    setError('')
    setBusy(true)
    const list = Array.isArray(free) ? free.filter((l): l is string => typeof l === 'string') : []
    try {
      await setSetting(host.client, FREE_KEY, (mayUse ? list.filter((l) => !sameRepo(l, repo)) : [...list, repo]) as never)
    } catch (e) {
      setError(say(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="flex flex-col gap-4">
      {hasLevels(settings) ? (
        <div className="flex flex-col gap-1">
          <Field label="Autonomy" hint="Every new run here starts from this: a lead never merges a PR you merge, and leaves a plan you approve to you. A run started with a level of its own keeps it.">
            <Select value={level} disabled={busy} onChange={(e) => void choose(e.target.value as Level)}>
              {(Object.keys(LEVEL_LABEL) as Level[]).map((l) => <option key={l} value={l}>{LEVEL_LABEL[l]}</option>)}
            </Select>
          </Field>
          {byName.length > 0 && (
            <p className="text-xs text-muted-foreground">
              {byName.map((e) => `"${e}"`).join(', ')} in the host's settings names every project of that folder name, so it holds this one too: edit it there to lower the level.
            </p>
          )}
        </div>
      ) : (
        <p className="text-xs text-muted-foreground">This host's daemon has no autonomy settings yet.</p>
      )}
      {settings[FREE_KEY] && (
        <label className="flex items-start gap-2 text-[13px]">
          <input type="checkbox" className="mt-0.5" aria-label="Free models allowed" checked={!kept} disabled={busy} onChange={(e) => void setFree(e.target.checked)} />
          <span>
            Free models allowed
            <span className="block text-xs text-muted-foreground">Some of OpenCode's free models are served by providers that keep prompts or train on them. Off: nothing here starts on, switches to or is reviewed by a zero-cost model.</span>
          </span>
        </label>
      )}
      <label className="flex items-center gap-2 text-[13px]">
        <input type="checkbox" aria-label="Pinned in the sidebar" checked={pinned} onChange={() => app.togglePin(host.info.id, repo)} />
        Pinned to the top of the sidebar (on this device)
      </label>
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <div className="flex justify-end">
        <Button variant="secondary" onClick={() => app.openProjectSettings(undefined)}>Done</Button>
      </div>
    </div>
  )
}
