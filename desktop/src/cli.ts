/** The CLIs a host runs agents on, and how the app names them. */

/** The CLI kinds in the usual order, then any other a host defines. */
const ORDER = ['claude', 'agy', 'opencode', 'copilot']

const NAMES: Record<string, string> = { claude: 'Claude', agy: 'agy', opencode: 'OpenCode', copilot: 'Copilot' }

/** The name a CLI is shown by ("Claude", "agy"); an unknown kind as it is. */
export const cliName = (kind: string): string => NAMES[kind] ?? kind

/**
 * The CLIs a host has set up: those its settings give tiers (`tiers.<cli>.<tier>.*`), as the New workspace dialog's tiers
 * are read. A host that lists none yet (not loaded, or an older daemon) offers Claude and agy.
 */
export function cliKinds(settings: Record<string, unknown>): string[] {
  const found = new Set<string>()
  for (const key of Object.keys(settings)) {
    const m = /^tiers\.([^.]+)\.[^.]+\./.exec(key)
    if (m) found.add(m[1]!)
  }
  if (found.size === 0) return ['claude', 'agy']
  return [...found].sort((a, b) => (ORDER.indexOf(a) < 0 ? 99 : ORDER.indexOf(a)) - (ORDER.indexOf(b) < 0 ? 99 : ORDER.indexOf(b)) || a.localeCompare(b))
}
