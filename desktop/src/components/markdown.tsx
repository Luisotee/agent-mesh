import type { ComponentProps } from 'react'
import remarkBreaks from 'remark-breaks'
import { Streamdown, defaultRehypePlugins, defaultRemarkPlugins } from 'streamdown'
import { highlighter } from '@/highlight'
import { cn } from '@/lib/utils'

type El<T extends keyof React.JSX.IntrinsicElements> = Omit<ComponentProps<T>, 'ref' | 'node'> & { node?: unknown }

/** Code blocks highlighted by Shiki (`highlight.ts`: its JavaScript regex engine, no WASM, a grammar loaded when first shown). */
const plugins = { code: highlighter }
/** Streamdown's rehype plugins minus `raw`, which would turn raw HTML into elements. */
const { raw: _raw, ...kept } = defaultRehypePlugins
const rehypePlugins = Object.values(kept)

interface MdNode {
  type: string
  children?: MdNode[]
}
/** Drops raw HTML from the markdown tree (Streamdown's `skipHtml` would leave it as escaped text): no tag shows. */
function dropHtml(node: MdNode): void {
  if (!node.children) return
  node.children = node.children.filter((c) => c.type !== 'html')
  node.children.forEach(dropHtml)
}
const remarkDropHtml = () => dropHtml
const remarkPlugins = [...Object.values(defaultRemarkPlugins), remarkDropHtml]
/** A prompt's own line breaks (Shift+Enter) kept, as they were typed; a reply's newlines join, as in any markdown. */
const breaking = [...remarkPlugins, remarkBreaks]
const controls = { code: { copy: true, download: false }, table: false, mermaid: false } as const

/** Opens a link in the user's browser, through the main process, and only an http(s) one. */
function open(href: string | undefined) {
  if (href && /^https?:\/\//i.test(href)) window.desktop?.openExternal(href)
}

const components = {
  a: ({ href, children: c }: El<'a'>) => (
    <a
      href={href}
      className="text-primary underline underline-offset-2"
      onClick={(e) => {
        e.preventDefault()
        open(href)
      }}
    >
      {c}
    </a>
  ),
  img: () => null,
  h1: ({ children: c }: El<'h1'>) => <h3 className="mt-2 text-lg font-semibold">{c}</h3>,
  h2: ({ children: c }: El<'h2'>) => <h4 className="mt-2 text-[15px] font-semibold">{c}</h4>,
  h3: ({ children: c }: El<'h3'>) => <h5 className="mt-1 font-semibold">{c}</h5>,
  h4: ({ children: c }: El<'h4'>) => <h6 className="font-semibold">{c}</h6>,
  p: ({ children: c }: El<'p'>) => <p>{c}</p>,
  strong: ({ children: c }: El<'strong'>) => <strong className="font-semibold">{c}</strong>,
  ul: ({ children: c }: El<'ul'>) => <ul className="list-disc pl-5">{c}</ul>,
  ol: ({ children: c }: El<'ol'>) => <ol className="list-decimal pl-5">{c}</ol>,
  li: ({ children: c }: El<'li'>) => <li className="my-0.5">{c}</li>,
  blockquote: ({ children: c }: El<'blockquote'>) => <blockquote className="border-l-2 pl-3 text-muted-foreground">{c}</blockquote>,
  hr: () => <hr className="my-2" />,
  inlineCode: ({ children: c }: El<'code'>) => <code className="rounded bg-surface px-1 font-mono text-[0.9em]">{c}</code>,
  table: ({ children: c }: El<'table'>) => <table className="w-full border-collapse text-[13px] tabular-nums">{c}</table>,
  th: ({ children: c }: El<'th'>) => <th className="border px-2 py-1 text-left font-semibold">{c}</th>,
  td: ({ children: c }: El<'td'>) => <td className="border px-2 py-1">{c}</td>,
}

/**
 * Markdown, as agents write it: replies, prompts, plans and reports. Streamdown renders it, so a reply cut mid-way
 * (an open code fence, a half-written list) still reads right. Raw HTML is dropped (not rendered, not escaped into
 * the page), images aren't loaded (the page's CSP allows none from outside), and a link opens in the user's browser,
 * through the main process, never in the app's own window.
 */
export function Markdown({ children, className, breaks }: { children: string; className?: string; /** Keep each line break (a prompt someone typed). */ breaks?: boolean }) {
  return (
    <Streamdown
      className={cn('flex flex-col gap-2 text-sm leading-relaxed break-words [&_pre]:font-mono [&_pre]:text-[13px]', className)}
      skipHtml
      rehypePlugins={rehypePlugins}
      remarkPlugins={breaks ? breaking : remarkPlugins}
      mode="static"
      lineNumbers={false}
      plugins={plugins}
      controls={controls}
      components={components}
    >
      {children}
    </Streamdown>
  )
}
