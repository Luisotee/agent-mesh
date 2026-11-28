import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { PromptOptionSchema, PromptQuestionSchema } from './gen/agentmesh/v1/chat_pb'
import { PendingPermissionSchema } from './gen/agentmesh/v1/mesh_pb'
import { answersFromResult, answersOf, hasPreviews, isComplete, pickOption, previewOf, promptsFor, writeOther, type Picks } from './prompts'

const opt = (label: string, preview = '') => create(PromptOptionSchema, { label, description: `${label}.`, preview })
const color = create(PromptQuestionSchema, { question: 'Which color do you want?', header: 'Color', options: [opt('Red', 'RED'), opt('Blue', 'BLUE')] })
const toppings = create(PromptQuestionSchema, { question: 'Which toppings do you want?', header: 'Toppings', multiSelect: true, options: [opt('Cheese'), opt('Ham')] })

describe('answering a question', () => {
  it('a single choice replaces the option picked, and writing under Other replaces the option', () => {
    let p: Picks = pickOption({}, color, 'Red')
    p = pickOption(p, color, 'Blue')
    expect(p[color.question]).toEqual({ labels: ['Blue'], other: '' })
    p = writeOther(p, color, 'Purple')
    expect(p[color.question]).toEqual({ labels: [], other: 'Purple' })
    // Picking an option again drops the written text: one answer.
    expect(pickOption(p, color, 'Red')[color.question]).toEqual({ labels: ['Red'], other: '' })
    // Clearing the text keeps what was picked.
    expect(writeOther(pickOption({}, color, 'Red'), color, '  ')[color.question]).toEqual({ labels: ['Red'], other: '  ' })
  })

  it('a multi-select toggles its options, and the Other text joins them', () => {
    let p = pickOption({}, toppings, 'Cheese')
    p = pickOption(p, toppings, 'Ham')
    p = writeOther(p, toppings, 'jam')
    expect(p[toppings.question]).toEqual({ labels: ['Cheese', 'Ham'], other: 'jam' })
    expect(pickOption(p, toppings, 'Cheese')[toppings.question]).toEqual({ labels: ['Ham'], other: 'jam' })
  })

  it('is complete only when every question has an option or text, and sends the text trimmed', () => {
    const qs = [color, toppings]
    expect(isComplete(qs, {})).toBe(false)
    const one = pickOption({}, color, 'Red')
    expect(isComplete(qs, one)).toBe(false)
    expect(isComplete(qs, writeOther(one, toppings, '  '))).toBe(false)
    const all = writeOther(one, toppings, ' jam ')
    expect(isComplete(qs, all)).toBe(true)
    expect(answersOf(qs, all)).toEqual([
      { question: color.question, labels: ['Red'], other: '' },
      { question: toppings.question, labels: [], other: 'jam' },
    ])
  })

  it('shows the preview of the option looked at, else the one chosen, else the first', () => {
    expect(hasPreviews(color)).toBe(true)
    expect(hasPreviews(toppings)).toBe(false)
    expect(previewOf(color, {})).toBe('RED')
    expect(previewOf(color, pickOption({}, color, 'Blue'))).toBe('BLUE')
    expect(previewOf(color, pickOption({}, color, 'Blue'), 'Red')).toBe('RED')
  })
})

describe('a question already asked', () => {
  const qs = [color, toppings]
  it('reads the answers out of the result Claude recorded, from the terminal or from an app', () => {
    for (const lead of ['Your questions have been answered: ', 'The user answered: ']) {
      const text = `${lead}"Which color do you want?"="Red", "Which toppings do you want?"="Cheese, Ham". You can now continue with these answers in mind.`
      expect([...answersFromResult(text, qs)]).toEqual([[color.question, 'Red'], [toppings.question, 'Cheese, Ham']])
    }
  })
  it('keeps a quote or a comma in an answer, and gives nothing for a result it cannot read', () => {
    const text = 'The user answered: "Which color do you want?"="a \\"deep\\", dark one", "Which toppings do you want?"="Ham". Read the answers carefully.'
    expect(answersFromResult(text, qs).get(toppings.question)).toBe('Ham')
    expect(answersFromResult(text, qs).get(color.question)).toContain('deep')
    const quoted = 'The user answered: "Which color do you want?"="Red", "Which toppings do you want?"="Use \\"v2\\". Then deploy". You can now continue.'
    expect(answersFromResult(quoted, qs).get(toppings.question)).toContain('Then deploy')
    expect(answersFromResult('The user dismissed the question.', qs).size).toBe(0)
  })
})

describe('the prompts of an agent', () => {
  it('are the ones only the user answers', () => {
    const a = create(PendingPermissionSchema, { id: 'p1', agentId: 'a', forUser: true })
    const b = create(PendingPermissionSchema, { id: 'p2', agentId: 'a', forUser: false, parentId: 'lead' })
    const c = create(PendingPermissionSchema, { id: 'p3', agentId: 'b', forUser: true })
    expect(promptsFor([a, b, c], 'a').map((p) => p.id)).toEqual(['p1'])
  })
})
