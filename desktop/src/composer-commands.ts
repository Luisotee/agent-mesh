import { CommandDialog, type AgentCommand } from './gen/agentmesh/v1/chat_pb'

/**
 * The composer's `/` completion, pure: when the popup opens, how it filters, what completing writes and what typing a
 * command does (a dialog the chat can't show, a command that ends the CLI). The commands are the host's
 * (`ListAgentCommands`): the app keeps no list of its own.
 */

/** The text of the command being typed (what follows the `/`, up to the caret), or undefined when the box is not on one: a `/` at the very start of the box and no space before the caret. */
export function slashQuery(value: string, caret: number): string | undefined {
  if (!value.startsWith('/') || caret < 1) return undefined
  const typed = value.slice(1, caret)
  return /\s/.test(typed) ? undefined : typed
}

const norm = (s: string) => s.toLowerCase()

/** How well a command matches what was typed: 0 is its name, 1 a name's start, 2 an alias's start, 3 a plugin command's own name's start, 4 inside its name, 5 inside its description; -1 not at all. */
function rank(c: AgentCommand, q: string): number {
  if (!q) return 1
  const name = norm(c.name)
  if (name === q) return 0
  if (name.startsWith(q)) return 1
  if (c.aliases.some((a) => norm(a).startsWith(q))) return 2
  const own = name.slice(name.indexOf(':') + 1)
  if (own !== name && own.startsWith(q)) return 3
  if (name.includes(q)) return 4
  // A word of the description only from four letters on: "con" would find every description with "conversation" in it.
  if (q.length >= 4 && norm(c.description).includes(q)) return 5
  return -1
}

/** The commands that match what was typed, the best match first and alphabetical within a kind of match. */
export function filterCommands(commands: AgentCommand[], typed: string): AgentCommand[] {
  const q = norm(typed)
  return commands
    .map((c) => ({ c, r: rank(c, q) }))
    .filter((x) => x.r >= 0)
    .sort((a, b) => a.r - b.r || a.c.name.localeCompare(b.c.name))
    .map((x) => x.c)
}

/** The box with the command's name written in place of the one being typed (the arguments after it stay), and where the caret goes: after the name and a space. */
export function complete(value: string, c: AgentCommand): { value: string; caret: number } {
  const rest = value.replace(/^\/\S*/, '')
  const gap = rest.startsWith(' ') || rest.startsWith('\n') ? '' : ' '
  const head = `/${c.name}${gap}`
  return { value: head + rest, caret: head.length + (gap ? 0 : 1) }
}

/** What the box holds when it is a command: the one it names (by name or alias) and what follows it, or undefined for any other text (a path, a `/word` the CLI doesn't know). */
export function invoked(text: string, commands: AgentCommand[]): { command: AgentCommand; args: string } | undefined {
  const m = /^\/(\S+)(?:\s+([\s\S]*))?$/.exec(text.trim())
  if (!m) return undefined
  const name = norm(m[1]!)
  const command = commands.find((c) => norm(c.name) === name) ?? commands.find((c) => c.aliases.some((a) => norm(a) === name))
  return command ? { command, args: (m[2] ?? '').trim() } : undefined
}

/** Whether typing this opens a dialog or picker in the agent's terminal, which the chat can't show. */
export function opensDialog(c: AgentCommand, args: string): boolean {
  return c.dialog === CommandDialog.ALWAYS || (c.dialog === CommandDialog.BARE && args === '')
}

/** How the popup reads a key: move, take the highlighted command, or let the box have it. */
export type PopupKey = { kind: 'move'; to: number } | { kind: 'complete' } | { kind: 'close' } | undefined

/**
 * Up and Down move through the list (and wrap); Tab takes the highlighted command; Enter takes it too, unless the box
 * already holds exactly its name, when Enter sends as it would anywhere; Esc closes the popup. Shift, Ctrl and Alt keys are the box's.
 */
export function popupKey(key: string, mods: { shift: boolean; mod: boolean; alt: boolean }, active: number, count: number, exact: boolean): PopupKey {
  if (count === 0 || mods.mod || mods.alt) return undefined
  if (key === 'ArrowDown') return { kind: 'move', to: (active + 1) % count }
  if (key === 'ArrowUp') return { kind: 'move', to: (active - 1 + count) % count }
  if (mods.shift) return undefined
  if (key === 'Tab') return { kind: 'complete' }
  if (key === 'Enter') return exact ? undefined : { kind: 'complete' }
  if (key === 'Escape') return { kind: 'close' }
  return undefined
}
