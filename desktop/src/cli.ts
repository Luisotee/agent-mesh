/** The CLIs a host runs agents on, and how the app names them. */

/** The CLI kinds in the usual order, then any other a host defines. */
const ORDER = ['claude', 'agy', 'opencode', 'copilot']

const NAMES: Record<string, string> = { claude: 'Claude', agy: 'agy', opencode: 'OpenCode', copilot: 'Copilot' }

/** The name a CLI is shown by ("Claude", "agy"); an unknown kind as it is. */
export const cliName = (kind: string): string => NAMES[kind] ?? kind

/** The CLIs a host says it can launch (`HelloResponse.clis`), or undefined from a daemon that does not say (no `clis` capability). */
export function hostClis(daemon: { capabilities: readonly string[]; clis: readonly string[] } | undefined): readonly string[] | undefined {
  return daemon?.capabilities.includes('clis') ? daemon.clis : undefined
}

/**
 * The CLIs a host can launch an agent on, in the usual order: those it says are installed (`hostClis`, an empty list too: none),
 * else, from a daemon that does not say, those its settings give tiers (`tiers.<cli>.<tier>.*`, as the New workspace dialog's tiers
 * are read), or Claude and agy while no settings are loaded.
 */
export function cliKinds(settings: Record<string, unknown>, installed?: readonly string[]): string[] {
  const rank = (k: string) => (ORDER.indexOf(k) < 0 ? 99 : ORDER.indexOf(k))
  const sorted = (kinds: Iterable<string>) => [...kinds].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
  if (installed) return sorted(installed)
  const found = new Set<string>()
  for (const key of Object.keys(settings)) {
    const m = /^tiers\.([^.]+)\.[^.]+\./.exec(key)
    if (m) found.add(m[1]!)
  }
  return found.size === 0 ? ['claude', 'agy'] : sorted(found)
}
