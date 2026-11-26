/**
 * Which vendor a CLI or a model belongs to, for the icon beside it. A CLI is known by its kind (`claude`, `agy`,
 * `opencode`, `copilot`, `codex`); a model by its name, since OpenCode and Copilot run other vendors' models.
 */
export type Vendor = 'claude' | 'gemini' | 'openai' | 'opencode' | 'copilot' | 'deepseek' | 'other'

const KIND: Record<string, Vendor> = { claude: 'claude', agy: 'gemini', opencode: 'opencode', copilot: 'copilot', codex: 'openai' }

/** The vendor of a CLI kind; `other` for one this app does not know. */
export const vendorOfKind = (kind: string): Vendor => KIND[kind.trim().toLowerCase()] ?? 'other'

/** The vendor of a model by its name or family (`claude-sonnet-5-5`, `opus`, `gemini-4-neon-high`, `gpt-5`), or `other`. */
export function vendorOfModel(model: string): Vendor {
  const m = model.trim().toLowerCase()
  if (!m) return 'other'
  if (/(^|[^a-z])(claude|opus|sonnet|haiku|fable)([^a-z]|$)/.test(m)) return 'claude'
  if (/gemini|neon|carbon|argon|hydrogen|flash|(^|[^a-z])pro([^a-z]|$)/.test(m)) return 'gemini'
  if (/^(gpt|o\d|codex|chatgpt)|[-/](gpt|codex)/.test(m)) return 'openai'
  if (/deepseek/.test(m)) return 'deepseek'
  return 'other'
}

export const VENDOR_NAME: Record<Vendor, string> = {
  claude: 'Anthropic Claude',
  gemini: 'Google Gemini',
  openai: 'OpenAI',
  opencode: 'OpenCode',
  copilot: 'GitHub Copilot',
  deepseek: 'DeepSeek',
  other: 'Unknown vendor',
}

/**
 * The vendor to show for an agent: its model's when the model says (OpenCode runs a model of another vendor), else its
 * CLI's.
 */
export function vendorOf(kind: string, model = ''): Vendor {
  const byKind = vendorOfKind(kind)
  // A CLI that runs only its own vendor's models says it by its kind; one that runs others' says it by the model.
  if (byKind === 'opencode' || byKind === 'copilot' || byKind === 'other') {
    const byModel = vendorOfModel(model)
    return byModel === 'other' ? byKind : byModel
  }
  return byKind
}
