import type { ThemeRegistration } from 'shiki'

const v = (name: string) => `var(--code-${name})`

/**
 * The one Shiki theme the chat's code uses: its colours are CSS variables, set from the app theme's terminal colours
 * (`theme.ts` `applyTheme`), so every theme, light or dark, colours code without a Shiki theme of its own.
 */
export const codeTheme: ThemeRegistration = {
  name: 'agent-mesh',
  type: 'dark',
  colors: { 'editor.foreground': 'var(--foreground)', 'editor.background': 'var(--surface)' },
  fg: 'var(--foreground)',
  bg: 'var(--surface)',
  tokenColors: [
    { scope: ['comment', 'punctuation.definition.comment'], settings: { foreground: 'var(--muted-foreground)', fontStyle: 'italic' } },
    { scope: ['keyword', 'storage', 'storage.type', 'keyword.operator.new', 'keyword.control'], settings: { foreground: v('magenta') } },
    { scope: ['string', 'string.quoted', 'string.template', 'markup.inline.raw'], settings: { foreground: v('green') } },
    { scope: ['constant.numeric', 'constant.language', 'constant.character', 'support.constant'], settings: { foreground: v('yellow') } },
    { scope: ['entity.name.function', 'support.function', 'meta.function-call'], settings: { foreground: v('blue') } },
    { scope: ['entity.name.type', 'entity.name.class', 'support.type', 'support.class', 'entity.other.inherited-class'], settings: { foreground: v('cyan') } },
    { scope: ['variable.parameter', 'entity.name.tag', 'meta.tag'], settings: { foreground: v('red') } },
    { scope: ['entity.other.attribute-name', 'support.type.property-name', 'meta.object-literal.key'], settings: { foreground: v('cyan') } },
    { scope: ['markup.inserted'], settings: { foreground: v('green') } },
    { scope: ['markup.deleted', 'invalid'], settings: { foreground: v('red') } },
    { scope: ['markup.heading', 'markup.bold'], settings: { fontStyle: 'bold' } },
  ],
}
