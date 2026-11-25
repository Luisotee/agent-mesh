import http from 'node:http'
import { connectWith } from './bridge.js'
import type { Redeemed } from './hosts.js'
import { Dialer, RouteError, type PairingLink } from './route.js'

/**
 * Redeems a pairing link's one-time token: `PairingService/RedeemPairing`, as Connect's JSON, over the link's
 * route (pinned TLS, through the relay when the direct address can't be reached). The call has no bearer token:
 * the one-time token is the credential, and it is spent by the first call that reaches the host. Main's own call,
 * not the renderer's: the bridge forwards only `MeshService` actions.
 */
export async function redeemPairing(link: PairingLink, deviceName: string): Promise<Redeemed> {
  const body = Buffer.from(JSON.stringify({ token: link.token, deviceName }))
  const dialer = new Dialer(link.route)
  const res = await new Promise<{ status: number; text: string }>((resolve, reject) => {
    const req = http.request({
      method: 'POST',
      path: '/agentmesh.v1.PairingService/RedeemPairing',
      headers: { host: 'meshd', 'content-type': 'application/json', 'content-length': String(body.byteLength) },
      createConnection: connectWith(dialer),
    })
    req.on('response', (r) => {
      const chunks: Buffer[] = []
      r.on('data', (c: Buffer) => chunks.push(c))
      r.on('end', () => resolve({ status: r.statusCode ?? 0, text: Buffer.concat(chunks).toString('utf8') }))
      r.on('error', reject)
    })
    req.on('error', reject)
    req.setTimeout(30_000, () => req.destroy(new RouteError('unreachable', 'the host did not answer')))
    req.end(body)
  }).catch((e: unknown) => {
    throw new Error(`couldn't reach the host to pair: ${e instanceof Error ? e.message : String(e)}`)
  })

  let json: Record<string, unknown> = {}
  try {
    json = JSON.parse(res.text) as Record<string, unknown>
  } catch {
    // Not JSON: the status says what happened.
  }
  if (res.status !== 200) {
    const why = typeof json.message === 'string' && json.message ? json.message : `the host answered ${res.status}`
    throw new Error(`the host refused the pairing: ${why}`)
  }
  const str = (k: string) => (typeof json[k] === 'string' ? (json[k] as string) : '')
  const token = str('deviceToken')
  if (!token || !str('deviceId')) throw new Error('the host answered without a device token')
  return { deviceId: str('deviceId'), deviceName: str('deviceName'), token, hostId: str('hostId'), hostName: str('hostName'), fingerprint: str('certFingerprint') }
}
