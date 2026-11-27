import type { ComponentProps } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { cn } from '@/lib/utils'

type El<T extends keyof React.JSX.IntrinsicElements> = Omit<ComponentProps<T>, 'ref' | 'node'> & { node?: unknown }

/**
 * Markdown as plans and reports are written. Raw HTML is dropped (not rendered, not escaped into the page),
 * images aren't loaded (the page's CSP allows none from outside), and a link opens in the user's browser,
 * through the main process, never in the app's own window.
 */
export function Markdown({ children, className }: { children: string; className?: string }) {
  return (
    <div className={cn('flex flex-col gap-2 text-sm leading-relaxed break-words', className)}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        components={{
          a: ({ href, children: c }: El<'a'>) => (
            <a
              href={href}
              className="text-primary underline underline-offset-2"
              onClick={(e) => {
                e.preventDefault()
                if (href && /^https?:\/\//i.test(href)) window.desktop?.openExternal(href)
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
          ul: ({ children: c }: El<'ul'>) => <ul className="list-disc pl-5">{c}</ul>,
          ol: ({ children: c }: El<'ol'>) => <ol className="list-decimal pl-5">{c}</ol>,
          li: ({ children: c }: El<'li'>) => <li className="my-0.5">{c}</li>,
          blockquote: ({ children: c }: El<'blockquote'>) => <blockquote className="border-l-2 pl-3 text-muted-foreground">{c}</blockquote>,
          hr: () => <hr className="my-2" />,
          pre: ({ children: c }: El<'pre'>) => <pre className="overflow-x-auto rounded-md border bg-surface p-2.5 font-mono text-[13px] [&_code]:bg-transparent [&_code]:px-0">{c}</pre>,
          code: ({ className: k, children: c }: El<'code'>) => <code className={cn('rounded bg-surface px-1 font-mono text-[0.9em]', k)}>{c}</code>,
          table: ({ children: c }: El<'table'>) => <table className="w-full border-collapse text-[13px] tabular-nums">{c}</table>,
          th: ({ children: c }: El<'th'>) => <th className="border px-2 py-1 text-left font-semibold">{c}</th>,
          td: ({ children: c }: El<'td'>) => <td className="border px-2 py-1">{c}</td>,
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  )
}
