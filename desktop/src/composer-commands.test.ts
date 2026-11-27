import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { complete, filterCommands, invoked, opensDialog, popupKey, slashQuery } from './composer-commands'
import { AgentCommandSchema, CommandDialog } from './gen/agentmesh/v1/chat_pb'

const cmd = (name: string, o: Partial<{ aliases: string[]; description: string; dialog: CommandDialog; danger: boolean; argumentHint: string }> = {}) => create(AgentCommandSchema, { name, ...o })
const commands = [
  cmd('compact', { description: 'Free up context by summarizing', argumentHint: '<focus>' }),
  cmd('config', { aliases: ['settings'], description: 'Open settings', dialog: CommandDialog.BARE }),
  cmd('context', { description: 'Show what fills the context' }),
  cmd('resume', { dialog: CommandDialog.ALWAYS }),
  cmd('exit', { aliases: ['quit'], danger: true }),
  cmd('agent-mesh:delegate', { description: 'hands work to agents' }),
]

describe('slashQuery', () => {
  it('opens on a slash at the very start of the box, up to the caret', () => {
    expect(slashQuery('/', 1)).toBe('')
    expect(slashQuery('/com', 4)).toBe('com')
    expect(slashQuery('/compact', 4)).toBe('com') // the caret in the middle of the word
  })
  it('stays shut anywhere else', () => {
    expect(slashQuery('', 0)).toBeUndefined()
    expect(slashQuery('fix /compact', 12)).toBeUndefined()
    expect(slashQuery(' /compact', 9)).toBeUndefined()
    expect(slashQuery('/compact now', 12)).toBeUndefined() // past the name: arguments
    expect(slashQuery('/compact\nmore', 13)).toBeUndefined()
    expect(slashQuery('/compact', 0)).toBeUndefined()
  })
})

describe('filterCommands', () => {
  const names = (typed: string) => filterCommands(commands, typed).map((c) => c.name)
  it('lists everything for a bare slash, alphabetically', () => {
    expect(names('')).toEqual(['agent-mesh:delegate', 'compact', 'config', 'context', 'exit', 'resume'])
  })
  it('puts a name before an alias before a plugin command before a substring before a description', () => {
    expect(names('co')).toEqual(['compact', 'config', 'context'])
    expect(names('set')).toEqual(['config']) // an alias
    expect(names('deleg')).toEqual(['agent-mesh:delegate']) // the plugin command's own name
    expect(names('sume')).toEqual(['resume']) // inside the name
    expect(names('summar')).toEqual(['compact']) // inside the description
  })
  it('is not case sensitive and finds nothing for nothing', () => {
    expect(names('COMP')).toEqual(['compact'])
    expect(names('zzz')).toEqual([])
  })
  it('puts an exact name first', () => {
    expect(filterCommands([cmd('computer'), cmd('comp')], 'comp').map((c) => c.name)).toEqual(['comp', 'computer'])
  })
})

describe('complete', () => {
  it('writes the name and a space, keeping what follows', () => {
    expect(complete('/com', commands[0]!)).toEqual({ value: '/compact ', caret: 9 })
    expect(complete('/com keep the plan', commands[0]!)).toEqual({ value: '/compact keep the plan', caret: 9 })
    expect(complete('/', commands[4]!)).toEqual({ value: '/exit ', caret: 6 })
  })
})

describe('invoked', () => {
  it('knows a command by name or alias, with its arguments', () => {
    expect(invoked('/compact keep the plan', commands)).toMatchObject({ command: { name: 'compact' }, args: 'keep the plan' })
    expect(invoked('/quit', commands)).toMatchObject({ command: { name: 'exit' }, args: '' })
    expect(invoked('  /RESUME  ', commands)?.command.name).toBe('resume')
  })
  it('leaves a path, an unknown word and plain text as an ordinary prompt', () => {
    expect(invoked('/etc/hosts is wrong', commands)).toBeUndefined()
    expect(invoked('/nope', commands)).toBeUndefined()
    expect(invoked('please /compact', commands)).toBeUndefined()
    expect(invoked('/', commands)).toBeUndefined()
  })
})

describe('opensDialog', () => {
  it('is always for a dialog command and only bare for a picker that takes an argument', () => {
    expect(opensDialog(cmd('resume', { dialog: CommandDialog.ALWAYS }), 'abc')).toBe(true)
    expect(opensDialog(cmd('model', { dialog: CommandDialog.BARE }), '')).toBe(true)
    expect(opensDialog(cmd('model', { dialog: CommandDialog.BARE }), 'opus')).toBe(false)
    expect(opensDialog(cmd('compact'), '')).toBe(false)
  })
})

describe('popupKey', () => {
  const none = { shift: false, mod: false, alt: false }
  it('moves with the arrows and wraps', () => {
    expect(popupKey('ArrowDown', none, 2, 3, false)).toEqual({ kind: 'move', to: 0 })
    expect(popupKey('ArrowUp', none, 0, 3, false)).toEqual({ kind: 'move', to: 2 })
  })
  it('takes the highlighted command with Tab or Enter, but sends on Enter when the name is already typed', () => {
    expect(popupKey('Tab', none, 1, 3, false)).toEqual({ kind: 'complete' })
    expect(popupKey('Enter', none, 1, 3, false)).toEqual({ kind: 'complete' })
    expect(popupKey('Enter', none, 1, 3, true)).toBeUndefined()
    expect(popupKey('Escape', none, 1, 3, false)).toEqual({ kind: 'close' })
  })
  it('leaves modified keys and an empty list to the box', () => {
    expect(popupKey('Enter', { ...none, shift: true }, 0, 3, false)).toBeUndefined()
    expect(popupKey('Tab', { ...none, mod: true }, 0, 3, false)).toBeUndefined()
    expect(popupKey('Enter', none, 0, 0, false)).toBeUndefined()
  })
})
