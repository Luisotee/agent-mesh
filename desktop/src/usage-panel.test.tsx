import { create } from '@bufbuild/protobuf'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { UsagePanel } from './components/usage-panel'
import { AppContext } from './context'
import { GetUsageResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { GetCostResponseSchema } from './gen/agentmesh/v1/report_pb'
import { RunSchema } from './gen/agentmesh/v1/types_pb'
import type { HostRuntime } from './model'
import { createAppModel } from './model'
import { createHostStore } from './store'

function hostOf(id: string, client: unknown) {
  const store = createHostStore()
  store.setState({ runs: {} })
  return { info: { id, name: id, kind: 'state-dir', target: '/s' }, store, client } as unknown as HostRuntime
}

function modelOf() {
  return createAppModel({ hosts: { list: async () => [], fixed: false }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as never)
}

function fixture(o: { usage?: () => Promise<unknown>; cost?: () => Promise<unknown> } = {}) {
  const store = createHostStore()
  store.setState({ runs: { r1: create(RunSchema, { id: 'r1', title: 'e2e run' }) } })
  const client = {
    getUsage: vi.fn(o.usage ?? (async () => create(GetUsageResponseSchema, { quotaStop: 90, minQuota: 10, claude: { source: 'live', atReserve: ['weekly window'], windows: [{ key: 'seven_day', label: 'weekly window', used: 95 }, { key: 'five_hour', label: 'five-hour window', used: 20 }] } }))),
    getCost: vi.fn(o.cost ?? (async () => create(GetCostResponseSchema, { scope: 'since 7d', usd: 4.5, totalTokens: 12_300_000n, totalRead: 90_000_000n, clis: [{ cli: 'claude', calls: 2n, input: 10_000_000n, w1: 1_000_000n, output: 1_300_000n, total: 12_300_000n, read: 90_000_000n, basis: 'exact: the API usage' }], kinds: [{ kind: 'claude', sessions: 1n, calls: 2n, usd: 4.5, share: 100, total: 12_300_000n }] }))),
  }
  const host = { info: { id: 'h', name: 'home', kind: 'state-dir', target: '/s' }, store, client } as unknown as HostRuntime
  const model = createAppModel({ hosts: { list: async () => [], fixed: false }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as never)
  return { client, ui: <AppContext.Provider value={model}><UsagePanel host={host} /></AppContext.Provider> }
}

describe('UsagePanel', () => {
  it('shows the quota windows with the reserve warning, and the cost of the last 7 days', async () => {
    const { ui, client } = fixture()
    render(ui)
    const week = await screen.findByRole('progressbar', { name: 'weekly window used' })
    expect(week).toHaveAttribute('aria-valuenow', '95')
    expect(screen.getByRole('alert')).toHaveTextContent('At the reserve (90% of a five-hour window, 90% of a weekly one): Claude weekly window')
    expect(document.querySelector('[data-window="five_hour"]')).not.toHaveTextContent('at the reserve')
    expect(client.getUsage).toHaveBeenCalledWith({ refresh: false })
    // Tokens are the main figure; the dollars follow, as what the Claude sessions would cost at API prices.
    await waitFor(() => expect(screen.getByTestId('cost-total')).toHaveTextContent('12.3M tokens'))
    expect(screen.getByTestId('cost-total')).not.toHaveTextContent('$')
    expect(screen.getByTestId('cost-usd')).toHaveTextContent('≈ $4.50 at API prices for Claude')
    expect(screen.getByText(/cache reads 90\.0M apart/)).toBeInTheDocument()
    expect(screen.getByText('claude: exact: the API usage')).toBeInTheDocument()
    expect(client.getCost).toHaveBeenCalledWith({ agents: false, runId: '', since: '7d' })
    expect(within(screen.getByRole('table', { name: 'By kind' })).getByText('claude')).toBeInTheDocument()
  })

  it('asks again for another span or one run, per agent, and refreshes past the host cache', async () => {
    const { ui, client } = fixture()
    render(ui)
    await screen.findByTestId('cost-total')
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Cost of' }), 'run:r1')
    await waitFor(() => expect(client.getCost).toHaveBeenLastCalledWith({ agents: false, runId: 'r1', since: '' }))
    await userEvent.click(screen.getByRole('checkbox', { name: 'Per agent' }))
    await waitFor(() => expect(client.getCost).toHaveBeenLastCalledWith({ agents: true, runId: 'r1', since: '' }))
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(client.getUsage).toHaveBeenLastCalledWith({ refresh: true }))
  })

  it('draws each window against its own reserve and says which are kept for PR reviews', async () => {
    const { ui } = fixture({
      usage: async () =>
        create(GetUsageResponseSchema, {
          quotaStop: 85,
          quotaStopWeekly: 95,
          reviewsOnlyClaude: 70,
          claude: {
            source: 'live',
            reviewsOnly: ['five-hour window'],
            windows: [
              { key: 'five_hour', label: 'five-hour window', used: 75, reserve: 85 },
              { key: 'seven_day', label: 'weekly window', used: 20, reserve: 95 },
            ],
          },
        }),
    })
    render(ui)
    await screen.findByRole('progressbar', { name: 'five-hour window used' })
    expect(document.querySelector('[data-window="five_hour"] [title="reserve: 85%"]')).not.toBeNull()
    expect(document.querySelector('[data-window="seven_day"] [title="reserve: 95%"]')).not.toBeNull()
    expect(screen.getByRole('status')).toHaveTextContent('Only PR reviews: Claude five-hour window')
    expect(screen.queryByRole('alert')).toBeNull()
    const text = document.body.textContent ?? ''
    expect(text).toContain('85% of a five-hour one (quota_stop) or 95% of a weekly one (quota_stop_weekly) is used')
    expect(text).toContain('Claude starts only PR reviews from 70% (quota_reviews_only.claude)')
  })

  it('says so when there is no Claude reading and nothing was spent, and shows the host\'s error', async () => {
    const { ui } = fixture({
      usage: async () => create(GetUsageResponseSchema, { quotaStop: 90 }),
      cost: async () => {
        throw new Error('[invalid_argument] name a run, or a window with --since')
      },
    })
    render(ui)
    expect(await screen.findByText(/No reading/)).toBeInTheDocument()
    expect(await screen.findByText('name a run, or a window with --since')).toBeInTheDocument()
  })

  it('shows a failed refresh beside the reading it did not replace', async () => {
    let fail = false
    const { ui, client } = fixture({
      usage: async () => {
        if (fail) throw new Error('[unavailable] host went away')
        return create(GetUsageResponseSchema, { quotaStop: 90, minQuota: 10, claude: { source: 'live', windows: [{ key: 'seven_day', label: 'weekly window', used: 30 }] } })
      },
    })
    render(ui)
    await screen.findByRole('progressbar', { name: 'weekly window used' })
    expect(screen.queryByText(/Last refresh failed/)).toBeNull()
    fail = true
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    expect(await screen.findByText(/Last refresh failed: host went away/)).toBeInTheDocument()
    expect(screen.getByRole('progressbar', { name: 'weekly window used' })).toBeInTheDocument() // the old reading stays
    fail = false
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(screen.queryByText(/Last refresh failed/)).toBeNull())
    expect(client.getUsage).toHaveBeenCalledTimes(3)
  })

  it('starts afresh when another host takes its place: no old reading, and the old run is not asked about', async () => {
    const a = hostOf('a', {
      getUsage: vi.fn(async () => create(GetUsageResponseSchema, { quotaStop: 90, minQuota: 10, claude: { source: 'live', windows: [{ key: 'seven_day', label: 'weekly window', used: 30 }] } })),
      getCost: vi.fn(async () => create(GetCostResponseSchema, { scope: 'since 7d', usd: 1 })),
    })
    a.store.setState({ runs: { r1: create(RunSchema, { id: 'r1', title: 'only on a' }) } })
    const b = hostOf('b', {
      getUsage: vi.fn(() => new Promise(() => {})), // never answers: B is connecting
      getCost: vi.fn(async () => create(GetCostResponseSchema, { scope: 'since 7d', usd: 2 })),
    })
    const model = modelOf()
    const { rerender } = render(<AppContext.Provider value={model}><UsagePanel host={a} /></AppContext.Provider>)
    await screen.findByRole('progressbar', { name: 'weekly window used' })
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Cost of' }), 'run:r1')
    rerender(<AppContext.Provider value={model}><UsagePanel host={b} /></AppContext.Provider>)
    expect(screen.queryByRole('progressbar', { name: 'weekly window used' })).toBeNull() // A's reading is gone
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Usage and cost of b')
    await waitFor(() => expect(b.client.getCost).toHaveBeenCalledWith({ agents: false, runId: '', since: '7d' }))
    expect(b.client.getCost).not.toHaveBeenCalledWith(expect.objectContaining({ runId: 'r1' }))
  })
})
