import type { OpenedLink } from '../shared/api.js'
import { parsePairingLink } from './route.js'

/** The URL scheme the app answers to: pairing links (`agentmesh://pair?...`). */
export const PROTOCOL = 'agentmesh'

/** A link the operating system handed the app, if it is a pairing link this app can read (a bad one is dropped). */
export function openedLink(text: string): OpenedLink | undefined {
  const link = text.trim()
  if (link.length > 4096 || !link.toLowerCase().startsWith(`${PROTOCOL}://`)) return undefined
  try {
    return { link, hostName: parsePairingLink(link).hostName }
  } catch {
    return undefined
  }
}

/** The last readable pairing link among a process's arguments (Windows and Linux pass the URL as one). */
export function linkIn(args: readonly string[]): OpenedLink | undefined {
  for (let i = args.length - 1; i >= 0; i--) {
    const l = openedLink(args[i]!)
    if (l) return l
  }
  return undefined
}

/**
 * Links that arrived from outside the app wait here until the window asks for them: the renderer may not be up yet
 * (a link that starts the app), and it is told with a ping when it is. Only the newest is kept.
 */
export class LinkInbox {
  private pending?: OpenedLink

  constructor(private readonly ping: () => void) {}

  offer(link: OpenedLink): void {
    this.pending = link
    this.ping()
  }

  take(): OpenedLink | undefined {
    const l = this.pending
    this.pending = undefined
    return l
  }
}
