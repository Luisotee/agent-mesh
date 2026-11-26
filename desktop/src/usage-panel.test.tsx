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

function fixture(o: { usage?: () => Promise<unknown>; cost?: () => Promise<unknown> } = {}) {
  const store = createHostStore()
  store.setState({ runs: { r1: create(RunSchema, { id: 'r1', title: 'e2e run' }) } })
  const client = {
    getUsage: vi.fn(o.usage ?? (async () => create(GetUsageResponseSchema, { quotaStop: 90, minQuota: 10, claude: { source: 'live', atReserve: ['weekly window'], windows: [{ key: 'seven_day', label: 'weekly window', used: 95 }, { key: 'five_hour', label: 'five-hour window', used: 20 }] } }))),
    getCost: vi.fn(o.cost ?? (async () => create(GetCostResponseSchema, { scope: 'since 7d', usd: 4.5, kinds: [{ kind: 'claude', sessions: 1n, calls: 2n, usd: 4.5, share: 100 }] }))),
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
    expect(screen.getByRole('alert')).toHaveTextContent('At the reserve (90% of a quota used): Claude weekly window')
    expect(document.querySelector('[data-window="five_hour"]')).not.toHaveTextContent('at the reserve')
    expect(client.getUsage).toHaveBeenCalledWith({ refresh: false })
    await waitFor(() => expect(screen.getByTestId('cost-total')).toHaveTextContent('$4.50'))
    expect(client.getCost).toHaveBeenCalledWith({ agents: false, runId: '', since: '7d' })
    expect(within(screen.getByRole('table', { name: 'By CLI' })).getByText('claude')).toBeInTheDocument()
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
})
