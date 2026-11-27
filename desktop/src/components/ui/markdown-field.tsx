import * as React from 'react'
import { composerKey, type Edit } from '@/composer-keys'
import { isMac } from '@/keys'
import { cn } from '@/lib/utils'
import { Textarea } from './input'

/** Applies an edit as typing would, so the field's undo (Ctrl+Z) takes it back; jsdom, which has no editing commands, gets the plain edit. */
function apply(el: HTMLTextAreaElement, e: Edit) {
  el.focus()
  el.setSelectionRange(e.from, e.to)
  const typed = e.text ? document.execCommand?.('insertText', false, e.text) : document.execCommand?.('delete')
  if (!typed) {
    el.setRangeText(e.text, e.from, e.to)
    el.dispatchEvent(new Event('input', { bubbles: true }))
  }
  el.setSelectionRange(e.selStart, e.selEnd)
}

export type MarkdownFieldProps = Omit<React.ComponentProps<'textarea'>, 'value' | 'onChange'> & {
  value: string
  onValueChange(v: string): void
  /** Enter sends (Shift+Enter adds a line); without it, Enter adds a line. */
  onSend?: () => void
  /** The tallest it grows to, as CSS (the chat's: 40% of the window). */
  maxHeight?: string
}

/**
 * A multi-line field that writes markdown (`composer-keys.ts`): Enter continues a list, Tab and Shift+Tab indent a list item or a selection,
 * Ctrl+B, I, E and K make bold, italic, code and a link (the app's shortcuts on those keys wait while it has the
 * focus; with Shift added they run), and it grows with its text. Every multi-line field a person writes to an agent in is one.
 */
export function MarkdownField({ value, onValueChange, onSend, maxHeight = '40vh', className, onKeyDown, ref, style, ...props }: MarkdownFieldProps) {
  const own = React.useRef<HTMLTextAreaElement>(null)
  React.useImperativeHandle(ref, () => own.current!, [])
  // Grow with the text, up to maxHeight, then scroll.
  const fit = React.useCallback(() => {
    const el = own.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${el.scrollHeight + 2}px`
  }, [])
  React.useLayoutEffect(fit, [value, fit])
  // Measured again when its width changes: a field drawn while hidden (the chat behind the terminal) measured nothing.
  React.useEffect(() => {
    const el = own.current
    if (!el || typeof ResizeObserver === 'undefined') return
    let width = -1
    const ro = new ResizeObserver(([e]) => {
      const w = e?.contentRect.width ?? 0
      if (w !== width) {
        width = w
        fit()
      }
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [fit])
  return (
    <Textarea
      {...props}
      ref={own}
      data-markdown-field=""
      value={value}
      rows={1}
      style={{ maxHeight, ...style }}
      className={cn('min-h-9 resize-none overflow-y-auto', className)}
      onChange={(e) => onValueChange(e.target.value)}
      onKeyDown={(e) => {
        onKeyDown?.(e)
        if (e.defaultPrevented || e.nativeEvent.isComposing) return
        const el = e.currentTarget
        const out = composerKey(
          { value: el.value, start: el.selectionStart, end: el.selectionEnd },
          { key: e.key, shift: e.shiftKey, mod: isMac ? e.metaKey : e.ctrlKey, alt: e.altKey },
          !!onSend,
        )
        if (!out) return
        e.preventDefault()
        if (out.kind === 'send') onSend?.()
        else apply(el, out.edit)
      }}
    />
  )
}
