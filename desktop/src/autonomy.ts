import { jsonOf } from './prefs'
import type { Setting } from './gen/agentmesh/v1/types_pb'

/**
 * The autonomy level of a project (`docs/apps.md`, "Autonomy per project"), kept as two host settings that list repos: a repo in
 * neither is handled as before ("default"), in `repos.user_merges` the user merges its PRs, in `repos.user_approves_plans`
 * the user also approves its plans. A repo is named in a list by its path or its folder's name, as the host takes `--repo`.
 */
export type Level = 'default' | 'merges' | 'plans'

export const MERGES_KEY = 'repos.user_merges'
export const PLANS_KEY = 'repos.user_approves_plans'

export const LEVEL_LABEL: Record<Level, string> = {
  default: 'Handled as before',
  merges: 'I merge its PRs',
  plans: 'I approve plans and merge',
}

export const listOf = (settings: Record<string, Setting>, key: string): string[] => list(settings, key)

const list = (settings: Record<string, Setting>, key: string): string[] => {
  const v = jsonOf(settings[key])
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
}

const trim = (p: string) => p.replace(/[/\\]+$/, '')
const name = (p: string) => trim(p).split(/[/\\]/).pop() ?? p
const exact = (entry: string, repo: string) => trim(entry) === trim(repo)
const names = (entry: string, repo: string) => exact(entry, repo) || trim(entry) === name(repo)

/**
 * The entries that cover the repo only by its folder's name (`api` for `/p/api`): they cover every repo of that name,
 * so the app sets the level by the repo's path and never edits them (the user's own words in the settings).
 */
export const nameEntries = (settings: Record<string, Setting>, repo: string): string[] =>
  [MERGES_KEY, PLANS_KEY].flatMap((k) => list(settings, k)).filter((e) => names(e, repo) && !exact(e, repo))

/** Whether the host has the two settings (an older host has not): without them no level can be kept. */
export const hasLevels = (settings: Record<string, Setting>) => !!settings[MERGES_KEY] && !!settings[PLANS_KEY]

/** The level of a repo: the stricter of the lists it is in. */
export function levelOf(settings: Record<string, Setting>, repo: string): Level {
  if (list(settings, PLANS_KEY).some((e) => names(e, repo))) return 'plans'
  if (list(settings, MERGES_KEY).some((e) => names(e, repo))) return 'merges'
  return 'default'
}

const RANK: Record<Level, number> = { default: 0, merges: 1, plans: 2 }

/** The level the folder-name entries alone give a repo. */
function byNameLevel(settings: Record<string, Setting>, repo: string): Level {
  const by = (key: string) => list(settings, key).some((e) => names(e, repo) && !exact(e, repo))
  return by(PLANS_KEY) ? 'plans' : by(MERGES_KEY) ? 'merges' : 'default'
}

/**
 * The two lists with the repo's own path taken out of both and put in the one for `level`, as whole values to set; only
 * what changes. An entry that names it by folder name stays (it covers other repos too): `nameEntries` says when one
 * still holds the repo at a level.
 */
export function withLevel(settings: Record<string, Setting>, repo: string, level: Level): Array<{ key: string; value: string[] }> {
  if (levelOf(settings, repo) === level) return []
  // A folder-name entry already holds the repo at a stricter level than the one chosen, and the app never edits it: a
  // path entry written now would change nothing until the user removes that entry.
  if (RANK[byNameLevel(settings, repo)] > RANK[level]) return []
  const next = (key: string, into: boolean) => {
    const cur = list(settings, key)
    const kept = cur.filter((e) => !exact(e, repo))
    return into ? [...kept, repo] : kept
  }
  const merges = next(MERGES_KEY, level === 'merges')
  const plans = next(PLANS_KEY, level === 'plans')
  const out: Array<{ key: string; value: string[] }> = []
  if (JSON.stringify(merges) !== JSON.stringify(list(settings, MERGES_KEY))) out.push({ key: MERGES_KEY, value: merges })
  if (JSON.stringify(plans) !== JSON.stringify(list(settings, PLANS_KEY))) out.push({ key: PLANS_KEY, value: plans })
  return out
}
