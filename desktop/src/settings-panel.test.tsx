import { create, fromJson, toJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'
import { SettingsPanel } from './components/settings-panel'
import { AppContext } from './context'
import { SettingSchema$ as SettingDescriptor, SettingSchemaSchema, type Setting } from './gen/agentmesh/v1/types_pb'
import { createAppModel, type HostRuntime } from './model'
import { createHostStore } from './store'

const setting = (key: string, type: string, def: unknown, value: unknown = def, o: { choices?: string[]; isSet?: boolean; summary?: string; restart?: boolean } = {}): Setting =>
  create(SettingDescriptor, {
    key,
    value: fromJson(ValueSchema, value as never),
    default: fromJson(ValueSchema, def as never),
    isSet: o.isSet ?? false,
    schema: create(SettingSchemaSchema, { type, summary: o.summary ?? `about ${key}`, choices: o.choices ?? [], restart: o.restart ?? false }),
  })

function fixture(list: Setting[]) {
  const store = createHostStore()
  store.setState({ settings: Object.fromEntries(list.map((s) => [s.key, s])) })
  const client = { setSetting: vi.fn(async (_r: unknown) => ({})), resetSetting: vi.fn(async (_r: unknown) => ({})) }
  const host = { info: { id: 'h', name: 'home', kind: 'state-dir', target: '/s' }, store, client, connection: { retryNow: vi.fn() } } as unknown as HostRuntime
  const model = createAppModel({ hosts: { list: async () => [], fixed: false }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi)
  const ui = <AppContext.Provider value={model}><SettingsPanel host={host} /></AppContext.Provider>
  return { host, client, store, ui }
}

const sentValue = (client: { setSetting: ReturnType<typeof vi.fn> }, n = 0) => {
  const req = client.setSetting.mock.calls[n]![0] as { key: string; value: Parameters<typeof toJson<typeof ValueSchema>>[1]; meta: { mutationId: string } }
  return { key: req.key, value: toJson(ValueSchema, req.value), mutation: req.meta.mutationId }
}

describe('SettingsPanel', () => {
  const list = () => [
    setting('app.notify.sound', 'bool', true),
    setting('app.notify.events', 'kinds', ['plan'], ['plan'], { choices: ['input', 'plan', 'question'] }),
    setting('quota_stop', 'percent', 90, 80, { isSet: true, summary: 'percent of quota used at which nothing new starts' }),
    setting('tiers.claude.mid.model', 'chain', ['sonnet']),
    setting('tiers.claude.mid.effort', 'effort', 'high', 'high', { choices: ['low', 'high'] }),
    setting('min_free_mb', 'mb_or_auto', 'auto', 'auto', { restart: true }),
    setting('review.reviewers.small', 'slots', [['agy']]),
  ]

  it('draws a control per type, with the schema text, the default and a restart note', () => {
    const { ui } = fixture(list())
    render(ui)
    const app = document.querySelector('[data-setting="app.notify.sound"]') as HTMLElement
    expect(within(app).getByRole('checkbox', { name: 'Value of app.notify.sound' })).toBeChecked()
    expect(screen.getByText('percent of quota used at which nothing new starts')).toBeInTheDocument()
    const quota = document.querySelector('[data-setting="quota_stop"]') as HTMLElement
    expect(within(quota).getByText('changed')).toBeInTheDocument()
    expect(quota).toHaveTextContent('Default: 90')
    expect(within(quota).getByRole('spinbutton', { name: 'Value of quota_stop' })).toHaveValue(80)
    expect(document.querySelector('[data-setting="min_free_mb"]')).toHaveTextContent('needs a restart')
  })

  it('sets a bool at once, with a mutation id', async () => {
    const { ui, client } = fixture(list())
    render(ui)
    await userEvent.click(screen.getByRole('checkbox', { name: 'Value of app.notify.sound' }))
    await waitFor(() => expect(client.setSetting).toHaveBeenCalledTimes(1))
    expect(sentValue(client)).toMatchObject({ key: 'app.notify.sound', value: false })
    expect(sentValue(client).mutation).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('sets a number when the field loses focus, and says why the host refused one', async () => {
    const { ui, client } = fixture(list())
    client.setSetting.mockRejectedValueOnce(new Error('[invalid_argument] quota_stop must be a whole number 0 to 100'))
    render(ui)
    const field = screen.getByRole('spinbutton', { name: 'Value of quota_stop' })
    await userEvent.clear(field)
    await userEvent.type(field, '120')
    fireEvent.blur(field)
    expect(await screen.findByRole('alert')).toHaveTextContent('quota_stop must be a whole number 0 to 100')
    expect(sentValue(client)).toMatchObject({ key: 'quota_stop', value: 120 })
  })

  it('puts a refused edit back to what the host has, and resets once on a double click', async () => {
    const { ui, client } = fixture(list())
    client.setSetting.mockRejectedValueOnce(new Error('[invalid_argument] nope'))
    render(ui)
    const select = screen.getByRole('combobox', { name: 'Value of tiers.claude.mid.effort' })
    await userEvent.selectOptions(select, 'low')
    expect(await screen.findByRole('alert')).toHaveTextContent(/^nope$/)
    await waitFor(() => expect(select).toHaveValue('high'))

    let release!: () => void
    client.resetSetting.mockImplementationOnce(() => new Promise((r) => (release = () => r({}))))
    const reset = screen.getByRole('button', { name: 'Reset quota_stop' })
    fireEvent.click(reset)
    fireEvent.click(reset)
    release()
    await waitFor(() => expect(client.resetSetting).toHaveBeenCalledTimes(1))
  })

  it('does not send an unchanged value, or a number that is not one', async () => {
    const { ui, client } = fixture(list())
    render(ui)
    const field = screen.getByRole('spinbutton', { name: 'Value of quota_stop' })
    fireEvent.blur(field)
    expect(client.setSetting).not.toHaveBeenCalled()
    const json = screen.getByRole('textbox', { name: 'Value of review.reviewers.small' })
    await userEvent.clear(json)
    await userEvent.type(json, '[[')
    fireEvent.blur(json)
    expect(await screen.findByRole('alert')).toHaveTextContent('not valid JSON')
    expect(client.setSetting).not.toHaveBeenCalled()
  })

  it('edits a set of choices, a chain by lines and an effort by select', async () => {
    const { ui, client } = fixture(list())
    render(ui)
    await userEvent.click(screen.getByRole('checkbox', { name: 'question' }))
    await waitFor(() => expect(client.setSetting).toHaveBeenCalledTimes(1))
    expect(sentValue(client, 0)).toMatchObject({ key: 'app.notify.events', value: ['plan', 'question'] })

    const chain = screen.getByRole('textbox', { name: 'Value of tiers.claude.mid.model' })
    await userEvent.clear(chain)
    await userEvent.type(chain, 'opus{enter}haiku')
    await userEvent.tab() // leaving the field commits it
    await waitFor(() => expect(client.setSetting).toHaveBeenCalledTimes(2))
    expect(sentValue(client, 1)).toMatchObject({ key: 'tiers.claude.mid.model', value: ['opus', 'haiku'] })

    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Value of tiers.claude.mid.effort' }), 'low')
    await waitFor(() => expect(client.setSetting).toHaveBeenCalledTimes(3))
    expect(sentValue(client, 2)).toMatchObject({ key: 'tiers.claude.mid.effort', value: 'low' })
  })

  it('resets a setting that was set', async () => {
    const { ui, client } = fixture(list())
    render(ui)
    await userEvent.click(screen.getByRole('button', { name: 'Reset quota_stop' }))
    expect(client.resetSetting).toHaveBeenCalledWith(expect.objectContaining({ key: 'quota_stop' }))
    expect(screen.queryByRole('button', { name: 'Reset app.notify.sound' })).toBeNull()
  })

  it('filters by name or description, and to the ones that were set', async () => {
    const { ui } = fixture(list())
    render(ui)
    await userEvent.type(screen.getByLabelText('Filter settings'), 'quota')
    expect(document.querySelector('[data-setting="quota_stop"]')).not.toBeNull()
    expect(document.querySelector('[data-setting="app.notify.sound"]')).toBeNull()
    await userEvent.clear(screen.getByLabelText('Filter settings'))
    await userEvent.click(screen.getByLabelText('Only the ones I set'))
    expect(document.querySelector('[data-setting="quota_stop"]')).not.toBeNull()
    expect(document.querySelector('[data-setting="tiers.claude.mid.model"]')).toBeNull()
  })

  it('takes a change made elsewhere (an event) into the field', async () => {
    const { ui, store } = fixture(list())
    render(ui)
    store.setState((s) => ({ settings: { ...s.settings, quota_stop: setting('quota_stop', 'percent', 90, 70, { isSet: true }) } }))
    await waitFor(() => expect(screen.getByRole('spinbutton', { name: 'Value of quota_stop' })).toHaveValue(70))
  })

  it('says so before the host has sent its settings', () => {
    const { ui } = fixture([])
    render(ui)
    expect(screen.getByText(/hasn't sent its settings yet/)).toBeInTheDocument()
  })
})
