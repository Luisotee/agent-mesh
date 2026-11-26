import { describe, expect, it } from 'vitest'
import { vendorOf, vendorOfKind, vendorOfModel } from './vendor'

describe('vendors', () => {
  it('knows a CLI by its kind', () => {
    expect(['claude', 'agy', 'opencode', 'copilot', 'codex', 'Claude ', 'mystery'].map(vendorOfKind)).toEqual(['claude', 'gemini', 'opencode', 'copilot', 'openai', 'claude', 'other'])
  })

  it('knows a model by its name or family', () => {
    const cases: Record<string, string> = {
      'claude-sonnet-5-5': 'claude', opus: 'claude', haiku: 'claude', 'anthropic/claude-opus-5-5': 'claude',
      'gemini-4-neon-high': 'gemini', neon: 'gemini', flash: 'gemini', 'gemini-3-pro': 'gemini',
      'gpt-5': 'openai', 'gpt-5-pro': 'openai', 'o3-pro': 'openai', 'openai/gpt-5-pro': 'openai', 'o3-mini': 'openai', 'openai/gpt-5': 'openai', codex: 'openai',
      'deepseek-v4': 'deepseek', 'deepseek-v4-flash': 'deepseek', 'deepseek-v4-pro': 'deepseek', '': 'other', 'llama-4': 'other',
    }
    for (const [m, v] of Object.entries(cases)) expect(vendorOfModel(m), m).toBe(v)
  })

  it('shows the model\'s vendor for a CLI that runs other vendors\' models, and the CLI\'s otherwise', () => {
    expect(vendorOf('opencode', 'deepseek-v4')).toBe('deepseek')
    expect(vendorOf('copilot', 'claude-sonnet-5-5')).toBe('claude')
    expect(vendorOf('opencode', '')).toBe('opencode')
    expect(vendorOf('claude', 'gpt-5')).toBe('claude')
    expect(vendorOf('agy', 'sonnet')).toBe('gemini')
    expect(vendorOf('unknown-cli', 'gemini-4')).toBe('gemini')
  })
})
