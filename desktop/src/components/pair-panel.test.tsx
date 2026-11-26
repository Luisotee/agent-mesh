import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError } from '@connectrpc/connect'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import jsQR from 'jsqr'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../../shared/api'
import { AppContext } from '@/context'
import { ActorClass } from '@/gen/agentmesh/v1/actor_pb'
import { CreatePairingResponseSchema, ListDevicesResponseSchema, TailnetStatusResponseSchema } from '@/gen/agentmesh/v1/mesh_pb'
import { DeviceSchema, EventSchema } from '@/gen/agentmesh/v1/types_pb'
import { createAppModel, type HostRuntime } from '@/model'
import { applyFrame, createHostStore } from '@/store'
import { PairPanel, canPair } from './pair-panel'
import { qrPath } from './qr-code'

const device = (id: string, name: string, cls = ActorClass.DEVICE) => create(DeviceSchema, { id, name, class: cls, createdAt: timestampFromMs(1_700_000_000_000) })
const URI = `agentmesh://pair?v=1&h=h_1&fp=sha256:${'ab'.repeat(32)}&t=amp_one&d=${encodeURIComponent('https://10.0.0.2:9443')}`

const tailnet = (init: Parameters<typeof create<typeof TailnetStatusResponseSchema>>[1] = {}) => create(TailnetStatusResponseSchema, init)

function fixture(caps: string[] = ['tls'], actorId = 'device-local', status = tailnet()) {
  const store = createHostStore()
  store.setState({ conn: { kind: 'ready', hello: { actor: { id: actorId, name: 'local' }, capabilities: caps, version: 'v' } as never }, daemon: { capabilities: caps } as never, synced: true })
  const client = {
    listDevices: vi.fn(async () => create(ListDevicesResponseSchema, { devices: [device('device-local', 'local'), device('d1', 'Pixel 9')] })),
    createPairing: vi.fn(async (_req: unknown) => create(CreatePairingResponseSchema, { uri: URI, expiresAt: timestampFromMs(Date.now() + 600_000) })),
    revokeDevice: vi.fn(async (_req: unknown) => ({})),
    tailnetStatus: vi.fn(async (_req: unknown) => status),
  }
  const host = { info: { id: 'h1', name: 'home', kind: 'state-dir', target: '/s' }, store, client } as unknown as HostRuntime
  const model = createAppModel({ hosts: { list: async () => [], fixed: false }, links: { take: async () => undefined, onOpen: vi.fn(() => () => {}) }, shell: { setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi)
  const wrap = (ui: React.ReactNode) => <AppContext.Provider value={model}>{ui}</AppContext.Provider>
  return { host, client, store, wrap }
}

describe('PairPanel', () => {
  it('makes a link with what was asked for, shows its QR code and link, and says when a device used it', async () => {
    const { host, client, store, wrap } = fixture()
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByText('Pixel 9')).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Name of the device'), 'Tablet')
    await userEvent.selectOptions(screen.getByLabelText('Link lasts'), '3600')
    await userEvent.type(screen.getByLabelText('Address the device reaches this host at'), 'https://10.0.0.2:9443')
    await userEvent.click(screen.getByLabelText(/Read-only/))
    await userEvent.click(screen.getByRole('button', { name: 'Create pairing link' }))
    await waitFor(() => expect(client.createPairing).toHaveBeenCalledTimes(1))
    const req = client.createPairing.mock.calls[0]![0] as { name: string; readOnly: boolean; ttlSeconds: bigint; directUrl: string; meta: { mutationId: string } }
    expect(req).toMatchObject({ name: 'Tablet', readOnly: true, ttlSeconds: 3600n, directUrl: 'https://10.0.0.2:9443' })
    expect(req.meta.mutationId).toBeTruthy()
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    expect(screen.getByLabelText('Pairing link')).toHaveValue(URI)

    // The device redeems it: the host announces DevicePaired, and the screen says so instead of showing the link.
    store.setState((s) => applyFrame(s, { case: 'event', value: create(EventSchema, { seq: 1n, generation: 'g', payload: { case: 'devicePaired', value: { $typeName: 'agentmesh.v1.DevicePaired', device: device('d2', 'Tablet', ActorClass.READ_ONLY_DEVICE), pairingId: '' } } }) }))
    expect(await screen.findByRole('status')).toHaveTextContent('Paired: Tablet')
    expect(screen.queryByRole('img', { name: 'Pairing QR code' })).toBeNull()
    expect(within(screen.getByRole('list', { name: 'Paired devices' })).getByText('read-only')).toBeInTheDocument()
  })

  const paired = (store: ReturnType<typeof fixture>['store'], seq: bigint, id: string, name: string, pairingId = '') =>
    store.setState((s) => applyFrame(s, { case: 'event', value: create(EventSchema, { seq, generation: 'g', payload: { case: 'devicePaired', value: { $typeName: 'agentmesh.v1.DevicePaired', device: device(id, name), pairingId } } }) }))

  it('matches the device that used this link by the pairing id, whatever it calls itself or what else appeared', async () => {
    const { host, client, store, wrap } = fixture()
    client.createPairing.mockImplementationOnce(async () => create(CreatePairingResponseSchema, { pairingId: 'p-mine', uri: URI, expiresAt: timestampFromMs(Date.now() + 600_000) }))
    render(wrap(<PairPanel host={host} />))
    await userEvent.type(await screen.findByLabelText('Name of the device'), 'Tablet')
    await userEvent.click(screen.getByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    // Another link's device appears first, one that is even named like ours; ours comes under another name.
    paired(store, 1n, 'd7', 'Tablet', 'p-other')
    expect(await screen.findByRole('note')).toHaveTextContent('Tablet paired meanwhile')
    expect(screen.queryByRole('status')).toBeNull()
    paired(store, 2n, 'd8', 'Galaxy', 'p-mine')
    expect(await screen.findByRole('status')).toHaveTextContent('Paired: Galaxy')
    expect(screen.queryByRole('img', { name: 'Pairing QR code' })).toBeNull()
  })

  it('never takes another link\'s only new device for its own, even with no name given', async () => {
    const { host, client, store, wrap } = fixture()
    client.createPairing.mockImplementationOnce(async () => create(CreatePairingResponseSchema, { pairingId: 'p-mine', uri: URI, expiresAt: timestampFromMs(Date.now() + 600_000) }))
    render(wrap(<PairPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    paired(store, 1n, 'd7', 'Phone', 'p-other')
    expect(await screen.findByRole('note')).toHaveTextContent('Phone paired meanwhile')
    expect(screen.queryByRole('status')).toBeNull()
    expect(screen.getByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
  })

  it('goes by name or the only new device when the host sends no pairing id (an older host)', async () => {
    const { host, client, store, wrap } = fixture()
    client.createPairing.mockImplementationOnce(async () => create(CreatePairingResponseSchema, { pairingId: 'p-mine', uri: URI, expiresAt: timestampFromMs(Date.now() + 600_000) }))
    render(wrap(<PairPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    paired(store, 1n, 'd9', 'Pixel 9') // no pairing id on the event
    expect(await screen.findByRole('status')).toHaveTextContent('Paired: Pixel 9')
  })

  it('still goes by name for a device the host said nothing about, when another link\'s device was seen', async () => {
    const { host, client, store, wrap } = fixture()
    client.createPairing.mockImplementationOnce(async () => create(CreatePairingResponseSchema, { pairingId: 'p-mine', uri: URI, expiresAt: timestampFromMs(Date.now() + 600_000) }))
    render(wrap(<PairPanel host={host} />))
    await userEvent.type(await screen.findByLabelText('Name of the device'), 'Tablet')
    await userEvent.click(screen.getByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    paired(store, 1n, 'd7', 'Phone', 'p-other')
    // Ours arrived while the stream was down: it is in the list, with no event behind it.
    store.setState((s) => ({ devices: [...s.devices, device('d8', 'Tablet')] }))
    expect(await screen.findByRole('status')).toHaveTextContent('Paired: Tablet')
  })

  it('forgets which link a revoked device came through', () => {
    const store = createHostStore()
    paired(store, 1n, 'd9', 'Pixel 9', 'p1')
    expect(store.getState().pairedVia).toEqual({ d9: 'p1' })
    store.setState((s) => applyFrame(s, { case: 'event', value: create(EventSchema, { seq: 2n, generation: 'g', payload: { case: 'deviceRevoked', value: { $typeName: 'agentmesh.v1.DeviceRevoked', deviceId: 'd9', name: 'Pixel 9' } } }) }))
    expect(store.getState().pairedVia).toEqual({})
  })

  it('with no name given, the one device that appears is the link\'s, whatever it calls itself', async () => {
    const { host, store, wrap } = fixture()
    render(wrap(<PairPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    paired(store, 1n, 'd9', 'Pixel 9')
    expect(await screen.findByRole('status')).toHaveTextContent('Paired: Pixel 9')
    expect(screen.queryByRole('img', { name: 'Pairing QR code' })).toBeNull()
    expect(screen.queryByRole('note')).toBeNull()
  })

  it('keeps the link when it can\'t be told which device used it, and says only who appeared', async () => {
    const { host, store, wrap } = fixture()
    render(wrap(<PairPanel host={host} />))
    await userEvent.type(await screen.findByLabelText('Name of the device'), 'Tablet')
    await userEvent.click(screen.getByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    // A name was given and this device has another (it named itself, or it paired some other way).
    paired(store, 1n, 'd9', 'Phone')
    const note = await screen.findByRole('note')
    expect(note).toHaveTextContent('Phone paired meanwhile')
    expect(note).not.toHaveTextContent('still works')
    expect(screen.getByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('with no name and two new devices, shows neither as the link\'s', async () => {
    const { host, store, wrap } = fixture()
    render(wrap(<PairPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    paired(store, 1n, 'd8', 'Phone')
    paired(store, 2n, 'd9', 'Laptop')
    expect(await screen.findByRole('note')).toHaveTextContent('Phone, Laptop paired meanwhile')
    expect(screen.getByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('reads the link\'s expiry on the host\'s clock: a host ahead of this machine does not make a fresh link expired', async () => {
    const { host, client, store, wrap } = fixture()
    const skew = 3 * 3600_000
    store.setState({ clockSkewMs: skew })
    client.createPairing.mockImplementationOnce(async () => create(CreatePairingResponseSchema, { uri: URI, expiresAt: timestampFromMs(Date.now() + skew + 600_000) }))
    render(wrap(<PairPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    expect(screen.queryByText(/expired/)).toBeNull()
  })

  it('and a host behind this machine expires the link when its own clock says so, not at once', async () => {
    const { host, client, store, wrap } = fixture()
    const skew = -3600_000
    store.setState({ clockSkewMs: skew })
    // 1.5 s left on the host's clock; read on this machine's, the link would be an hour old already.
    client.createPairing.mockImplementationOnce(async () => create(CreatePairingResponseSchema, { uri: URI, expiresAt: timestampFromMs(Date.now() + skew + 1500) }))
    render(wrap(<PairPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    expect(await screen.findByRole('status', {}, { timeout: 3000 })).toHaveTextContent(/expired/)
  })

  it('swaps the link for "expired" when its time has passed, and a new link starts afresh', async () => {
    const { host, client, wrap } = fixture()
    client.createPairing.mockImplementationOnce(async () => create(CreatePairingResponseSchema, { uri: URI, expiresAt: timestampFromMs(Date.now() + 1500) }))
    render(wrap(<PairPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    expect(await screen.findByRole('status', {}, { timeout: 3000 })).toHaveTextContent(/expired/)
    expect(screen.queryByRole('img', { name: 'Pairing QR code' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Copy/ })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Make another link' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
  })

  it('waits for the list of devices before it makes a link, so a device already paired is not taken for the new one', async () => {
    const { host, client, wrap } = fixture()
    let release!: () => void
    client.listDevices.mockImplementationOnce(() => new Promise((r) => (release = () => r(create(ListDevicesResponseSchema, { devices: [device('d1', 'Pixel 9')] })))))
    render(wrap(<PairPanel host={host} />))
    expect(screen.getByRole('button', { name: 'Create pairing link' })).toBeDisabled()
    release()
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    expect(screen.queryByRole('status')).toBeNull() // Pixel 9 was there already
  })

  it('copies the link', async () => {
    const { host, wrap } = fixture()
    const writeText = vi.fn(async () => {})
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    render(wrap(<PairPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Create pairing link' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Copy' }))
    expect(writeText).toHaveBeenCalledWith(URI)
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument()
  })

  it('revokes a device after a second click, and never offers it for the host\'s own device', async () => {
    const { host, client, store, wrap } = fixture()
    render(wrap(<PairPanel host={host} />))
    const list = await screen.findByRole('list', { name: 'Paired devices' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    expect(within(list).getAllByRole('button')).toHaveLength(1) // only the Pixel: the local device can't be revoked here
    await userEvent.click(within(list).getByRole('button', { name: 'Revoke access of Pixel 9' }))
    expect(client.revokeDevice).not.toHaveBeenCalled()
    await userEvent.click(within(list).getByRole('button', { name: 'Revoke Pixel 9' }))
    await waitFor(() => expect(client.revokeDevice).toHaveBeenCalledTimes(1))
    expect(client.revokeDevice.mock.calls[0]![0]).toMatchObject({ deviceId: 'd1' })
    await waitFor(() => expect(store.getState().devices.map((d) => d.id)).toEqual(['device-local']))
  })

  it('shows the host\'s refusal, and warns when the host can\'t be reached from another machine', async () => {
    const { host, client, wrap } = fixture([])
    client.createPairing.mockRejectedValueOnce(new Error('[permission_denied] only the host\'s own device may manage paired devices'))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByRole('note')).toHaveTextContent(/no direct TLS listener, no running Tailscale node and no relay/)
    await userEvent.click(screen.getByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('alert')).toHaveTextContent("only the host's own device may manage paired devices")
  })

  it('creates one link on a double click', async () => {
    const { host, client, wrap } = fixture()
    let release!: () => void
    client.createPairing.mockImplementationOnce(() => new Promise((r) => (release = () => r(create(CreatePairingResponseSchema, { uri: URI })))))
    render(wrap(<PairPanel host={host} />))
    const b = await screen.findByRole('button', { name: 'Create pairing link' })
    await userEvent.dblClick(b)
    release()
    await screen.findByRole('img', { name: 'Pairing QR code' })
    expect(client.createPairing).toHaveBeenCalledTimes(1)
  })
})

describe('canPair', () => {
  it('is true only for the host\'s own device', () => {
    expect(canPair(fixture().store)).toBe(true)
    expect(canPair(fixture(['tls'], 'device-abc').store)).toBe(false)
    const s = createHostStore()
    expect(canPair(s)).toBe(false)
  })
})

describe('qrPath', () => {
  it('draws the three finder squares and stays within its size', () => {
    const { size, path } = qrPath(URI)
    expect(size).toBeGreaterThan(21)
    expect(path.startsWith('M0 0h7v1h-7z')).toBe(true)
    for (const m of path.matchAll(/M(\d+) (\d+)h(\d+)v1/g)) expect(Number(m[1]) + Number(m[3])).toBeLessThanOrEqual(size)
  })
})

describe('the QR code', () => {
  it('scans back to the link it was made from', () => {
    // Draw the path's rectangles into a bitmap, black on white with the quiet zone, and read it as a camera would.
    const { size, path } = qrPath(URI)
    const scale = 6
    const side = (size + 8) * scale
    const px = new Uint8ClampedArray(side * side * 4).fill(255)
    for (const m of path.matchAll(/M(\d+) (\d+)h(\d+)v1/g)) {
      const [x, y, w] = [Number(m[1]), Number(m[2]), Number(m[3])]
      for (let dy = 0; dy < scale; dy++) {
        for (let dx = 0; dx < w * scale; dx++) {
          const i = (((y + 4) * scale + dy) * side + (x + 4) * scale + dx) * 4
          px[i] = px[i + 1] = px[i + 2] = 0
        }
      }
    }
    expect(jsQR(px, side, side)?.data).toBe(URI)
  })
})

describe('the Tailscale section of the pair screen', () => {
  it('says the node is off when meshd runs without --tailscale, and asks again slowly', async () => {
    const { host, client, wrap } = fixture()
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      render(wrap(<PairPanel host={host} />))
      const sec = await screen.findByRole('region', { name: 'Tailscale' })
      expect(sec).toHaveTextContent('--tailscale')
      expect(within(sec).queryByTestId('tailnet-state')).toBeNull()
      expect(client.tailnetStatus).toHaveBeenCalledTimes(1)
      // Restarted with --tailscale while the screen is open: the next slow poll shows it.
      client.tailnetStatus.mockResolvedValue(tailnet({ enabled: true, state: 'Starting' }))
      await vi.advanceTimersByTimeAsync(31_000)
      expect(await screen.findByTestId('tailnet-state')).toHaveTextContent('Starting')
    } finally {
      vi.useRealTimers()
    }
  })

  it('starts over for another host, and asks again after a reconnect', async () => {
    const a = fixture(['tls'], 'device-local', tailnet({ enabled: true, state: 'Running', hostname: 'node-a' }))
    const b = fixture(['tls'], 'device-local', tailnet())
    ;(b.host.info as { id: string }).id = 'h2'
    const { rerender } = render(a.wrap(<PairPanel host={a.host} />))
    expect(await screen.findByText('node-a')).toBeInTheDocument()
    // Another host takes the place of the panel without it unmounting (the pair screen stays open): its own answer shows.
    rerender(a.wrap(<PairPanel host={b.host} />))
    await waitFor(() => expect(b.client.tailnetStatus).toHaveBeenCalledTimes(1))
    expect(await screen.findByRole('region', { name: 'Tailscale' })).toHaveTextContent('--tailscale')
    expect(screen.queryByText('node-a')).toBeNull()
    // The same host reconnects (it may have been restarted with --tailscale): asked again at once.
    b.client.tailnetStatus.mockResolvedValue(tailnet({ enabled: true, state: 'Running', hostname: 'node-b' }))
    await act(async () => b.store.setState({ conn: { kind: 'connecting' } as never }))
    await act(async () => b.store.setState({ conn: { kind: 'ready', hello: { actor: { id: 'device-local', name: 'local' }, capabilities: ['tls'], version: 'v' } as never } }))
    expect(await screen.findByText('node-b')).toBeInTheDocument()
  })

  it('does not call a --tailscale-only host unreachable while its node runs', async () => {
    const { host, wrap } = fixture(['tailscale'], 'device-local', tailnet({ enabled: true, state: 'Running', directUrl: 'https://100.64.0.7:8443' }))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByTestId('tailnet-state')).toHaveTextContent('Connected')
    expect(screen.queryByRole('note')).toBeNull()
    expect(screen.getByText(/--listen address, or its tailnet address/)).toBeInTheDocument()
  })

  it('keeps the warning for a --tailscale host whose node is not running: it has no address for a link', async () => {
    const { host, wrap } = fixture(['tailscale'], 'device-local', tailnet({ enabled: true, state: 'NeedsLogin', authUrl: 'https://login.tailscale.com/a/abc' }))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByTestId('tailnet-state')).toHaveTextContent('Needs login')
    expect(screen.getByRole('note')).toHaveTextContent(/no running Tailscale node/)
  })

  it('drops an error from before the connection dropped once it is back', async () => {
    const { host, client, store, wrap } = fixture()
    client.tailnetStatus.mockRejectedValueOnce(new Error('[unavailable] connection reset'))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByText(/connection reset/)).toBeInTheDocument()
    client.tailnetStatus.mockImplementation(() => new Promise(() => {}))
    await act(async () => store.setState({ conn: { kind: 'connecting' } as never }))
    await act(async () => store.setState({ conn: { kind: 'ready', hello: { actor: { id: 'device-local', name: 'local' }, capabilities: ['tls'], version: 'v' } as never } }))
    await waitFor(() => expect(client.tailnetStatus).toHaveBeenCalledTimes(2))
    expect(screen.queryByText(/connection reset/)).toBeNull()
  })

  it('offers no address to use once the link is made (the form is gone)', async () => {
    const { host, wrap } = fixture(['tls'], 'device-local', tailnet({ enabled: true, state: 'Running', dnsName: 'm.t.ts.net.', directUrl: 'https://100.64.0.7:8443' }))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByRole('button', { name: /Use https:\/\/100.64.0.7:8443/ })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Create pairing link' }))
    expect(await screen.findByRole('img', { name: 'Pairing QR code' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /in the pairing link/ })).toBeNull()
  })

  it('shows the login link and its QR code while the node needs a login, and opens the link in the browser', async () => {
    const openExternal = vi.fn()
    ;(window as unknown as { desktop: unknown }).desktop = { openExternal }
    const { host, wrap } = fixture(['tls'], 'device-local', tailnet({ enabled: true, state: 'NeedsLogin', authUrl: 'https://login.tailscale.com/a/abc', hostname: 'mesh-home' }))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByTestId('tailnet-state')).toHaveTextContent('Needs login')
    expect(screen.getByRole('img', { name: 'Tailscale login QR code' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /Log in to Tailscale/ }))
    expect(openExternal).toHaveBeenCalledWith('https://login.tailscale.com/a/abc')
    // No address to offer yet.
    expect(screen.queryByRole('button', { name: /in the pairing link/ })).toBeNull()
  })

  it('never offers a login URL that is not https', async () => {
    const { host, wrap } = fixture(['tls'], 'device-local', tailnet({ enabled: true, state: 'NeedsLogin', authUrl: 'javascript:alert(1)' }))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByTestId('tailnet-state')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Log in to Tailscale/ })).toBeNull()
    expect(screen.queryByRole('img', { name: 'Tailscale login QR code' })).toBeNull()
  })

  it('lists the addresses of a running node and puts the chosen one into the pairing link form', async () => {
    const { host, client, wrap } = fixture(
      ['tls'],
      'device-local',
      tailnet({ enabled: true, state: 'Running', hostname: 'mesh-home', dnsName: 'mesh-home.tail1234.ts.net.', addresses: ['100.64.0.7', 'fd7a::7'], directUrl: 'https://100.64.0.7:8443', problems: ['health: no DERP'] }),
    )
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByTestId('tailnet-state')).toHaveTextContent('Connected')
    expect(screen.getByText('100.64.0.7, fd7a::7')).toBeInTheDocument()
    expect(screen.getByText('mesh-home.tail1234.ts.net')).toBeInTheDocument()
    expect(screen.getByRole('alert', { name: 'Tailscale problems' })).toHaveTextContent('no DERP')
    await userEvent.click(screen.getByRole('button', { name: 'Use https://mesh-home.tail1234.ts.net:8443 in the pairing link' }))
    expect(screen.getByLabelText('Address the device reaches this host at')).toHaveValue('https://mesh-home.tail1234.ts.net:8443')
    await userEvent.click(screen.getByRole('button', { name: 'Use https://100.64.0.7:8443 in the pairing link' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create pairing link' }))
    await waitFor(() => expect(client.createPairing).toHaveBeenCalledTimes(1))
    expect(client.createPairing.mock.calls[0]![0]).toMatchObject({ directUrl: 'https://100.64.0.7:8443' })
  })

  it('says nothing on a host that is too old to have the action', async () => {
    const { host, client, wrap } = fixture()
    client.tailnetStatus.mockRejectedValue(new ConnectError('unknown method', Code.Unimplemented))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByRole('button', { name: 'Create pairing link' })).toBeInTheDocument()
    await waitFor(() => expect(client.tailnetStatus).toHaveBeenCalledTimes(1))
    expect(screen.queryByRole('region', { name: 'Tailscale' })).toBeNull()
    expect(screen.queryByText(/Tailscale:/)).toBeNull()
  })

  it('shows why the host refused, without a panel of fake data', async () => {
    const { host, client, wrap } = fixture()
    client.tailnetStatus.mockRejectedValue(new Error('[permission_denied] only the host\'s own device may manage paired devices'))
    render(wrap(<PairPanel host={host} />))
    expect(await screen.findByText(/only the host's own device/)).toBeInTheDocument()
    expect(screen.queryByTestId('tailnet-state')).toBeNull()
  })
})
