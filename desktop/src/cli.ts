/** The CLIs a host runs agents on, and how the app names them. */

/** The CLI kinds in the usual order, then any other a host defines. */
const ORDER = ['claude', 'agy', 'opencode', 'copilot']

const NAMES: Record<string, string> = { claude: 'Claude', agy: 'agy', opencode: 'OpenCode', copilot: 'Copilot' }

/** The name a CLI is shown by ("Claude", "agy"); an unknown kind as it is. */
export const cliName = (kind: string): string => NAMES[kind] ?? kind

/**
 * The CLIs a host can launch an agent on: those its settings give tiers (`tiers.<cli>.<tier>.*`), as the New workspace dialog's
 * tiers are read, and that it says it has installed (`HelloResponse.clis`; a host that says none, an older daemon, is taken to
 * have them all). A host that lists no tiers yet (not loaded) offers Claude and agy, or the installed ones when it names them.
 */
export function cliKinds(settings: Record<string, unknown>, installed: readonly string[] = []): string[] {
  const found = new Set<string>()
  for (const key of Object.keys(settings)) {
    const m = /^tiers\.([^.]+)\.[^.]+\./.exec(key)
    if (m) found.add(m[1]!)
  }
  const rank = (k: string) => (ORDER.indexOf(k) < 0 ? 99 : ORDER.indexOf(k))
  const sorted = (kinds: Iterable<string>) => [...kinds].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
  if (found.size === 0) return installed.length ? sorted(installed) : ['claude', 'agy']
  return sorted([...found].filter((k) => installed.length === 0 || installed.includes(k)))
}
