import type { LanguageRegistration } from 'shiki'
import type { HighlighterCore } from 'shiki/core'
import type { CodeHighlighterPlugin, HighlightOptions, HighlightResult } from 'streamdown'
import { codeTheme } from './code-theme'

type Grammar = { default: LanguageRegistration[] }

/**
 * The languages the chat highlights, each its own chunk, loaded the first time a block of it shows. Others show as
 * plain text. A few names agents write map to them (`ts`, `sh`, `py`).
 */
const LANGS: Record<string, () => Promise<Grammar>> = {
  typescript: () => import('shiki/langs/typescript.mjs'),
  tsx: () => import('shiki/langs/tsx.mjs'),
  javascript: () => import('shiki/langs/javascript.mjs'),
  jsx: () => import('shiki/langs/jsx.mjs'),
  json: () => import('shiki/langs/json.mjs'),
  shellscript: () => import('shiki/langs/shellscript.mjs'),
  python: () => import('shiki/langs/python.mjs'),
  go: () => import('shiki/langs/go.mjs'),
  rust: () => import('shiki/langs/rust.mjs'),
  kotlin: () => import('shiki/langs/kotlin.mjs'),
  yaml: () => import('shiki/langs/yaml.mjs'),
  toml: () => import('shiki/langs/toml.mjs'),
  markdown: () => import('shiki/langs/markdown.mjs'),
  diff: () => import('shiki/langs/diff.mjs'),
  css: () => import('shiki/langs/css.mjs'),
  html: () => import('shiki/langs/html.mjs'),
  sql: () => import('shiki/langs/sql.mjs'),
  proto: () => import('shiki/langs/proto.mjs'),
}

const ALIASES: Record<string, string> = {
  ts: 'typescript', js: 'javascript', mjs: 'javascript', cjs: 'javascript', sh: 'shellscript', bash: 'shellscript', shell: 'shellscript', zsh: 'shellscript', console: 'shellscript',
  py: 'python', rs: 'rust', kt: 'kotlin', kts: 'kotlin', yml: 'yaml', md: 'markdown', patch: 'diff', protobuf: 'proto', golang: 'go',
}

/** The language a fence names, as the highlighter knows it, or undefined for one it doesn't. */
export function languageOf(name: string): string | undefined {
  const n = name.trim().toLowerCase()
  const id = ALIASES[n] ?? n
  return id in LANGS ? id : undefined
}

let core: Promise<HighlighterCore> | undefined
const loading = new Map<string, Promise<void>>()
const ready = new Set<string>()

function load(lang: string): Promise<void> {
  let p = loading.get(lang)
  if (!p) {
    // Shiki itself loads with the first code block, not with the app.
    core ??= Promise.all([import('shiki/core'), import('shiki/engine/javascript')]).then(([{ createHighlighterCore }, { createJavaScriptRegexEngine }]) =>
      createHighlighterCore({ themes: [codeTheme], langs: [], engine: createJavaScriptRegexEngine({ forgiving: true }) }),
    )
    core.catch(() => (core = undefined))
    p = core.then(async (h) => {
      await h.loadLanguage(...(await LANGS[lang]!()).default)
      ready.add(lang)
    })
    // A grammar that failed to load is tried again the next time.
    p.catch(() => loading.delete(lang))
    loading.set(lang, p)
  }
  return p
}

let sync: HighlighterCore | undefined
const tokens = (h: HighlighterCore, code: string, lang: string): HighlightResult => h.codeToTokens(code, { lang, theme: codeTheme.name! }) as HighlightResult

/** Streamdown's code plugin over the languages above: tokens at once when the grammar is loaded, else by `callback`. */
export const highlighter: CodeHighlighterPlugin = {
  name: 'shiki',
  type: 'code-highlighter',
  getSupportedLanguages: () => Object.keys(LANGS) as never,
  getThemes: () => [codeTheme, codeTheme],
  supportsLanguage: (lang) => !!languageOf(lang),
  highlight({ code, language }: HighlightOptions, callback) {
    const lang = languageOf(language)
    if (!lang) return null
    if (sync && ready.has(lang)) return tokens(sync, code, lang)
    void load(lang)
      .then(() => core!)
      .then((h) => {
        sync = h
        callback?.(tokens(h, code, lang))
      })
      .catch(() => {})
    return null
  },
}
