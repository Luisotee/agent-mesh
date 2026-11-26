import type { TailnetStatusResponse } from '@/gen/agentmesh/v1/mesh_pb'

/** Tailscale's backend states (`TailnetStatusResponse.state`) as the panel says them. */
const STATES: Record<string, string> = {
  Running: 'Connected',
  NeedsLogin: 'Needs login',
  NeedsMachineAuth: "Waiting for the tailnet's admin to approve this node",
  Starting: 'Starting',
  Stopped: 'Stopped',
  NoState: 'Starting',
}

export const tailnetStateLabel = (state: string): string => STATES[state] ?? (state || 'Unknown')

/** True once the node runs: its addresses and the pairing address are usable. */
export const tailnetRunning = (t: Pick<TailnetStatusResponse, 'enabled' | 'state'>): boolean => t.enabled && t.state === 'Running'

/** Only an http(s) login URL is offered as a link (it comes from the host, but it is opened in the user's browser). */
export const safeLoginUrl = (url: string): string | undefined => (/^https:\/\//i.test(url) && url.length < 2048 ? url : undefined)

/**
 * The addresses a pairing link for this host can name, best first: the node's own direct address (the IP works
 * whether or not the other device resolves MagicDNS), then its MagicDNS name on the same port.
 */
export function tailnetAddresses(t: Pick<TailnetStatusResponse, 'directUrl' | 'dnsName'>): string[] {
  const out: string[] = []
  if (t.directUrl) out.push(t.directUrl)
  const dns = t.dnsName.replace(/\.$/, '')
  if (dns && t.directUrl) {
    try {
      const u = new URL(t.directUrl)
      out.push(`https://${dns}${u.port ? `:${u.port}` : ''}`)
    } catch {
      // A malformed direct address from the host: offer only what parsed.
    }
  }
  return out
}
