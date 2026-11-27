/**
 * The markdown keys of a multi-line field (the chat's composer, a message, a PR comment, a task): pure, over the text
 * and the selection, so each is checked without a window. `MarkdownField` applies what they return as one edit, which
 * the field's own undo takes back.
 */

export interface Field {
  value: string
  start: number
  end: number
}

export interface KeyPress {
  key: string
  shift: boolean
  /** Ctrl, or Cmd on macOS. */
  mod: boolean
  alt: boolean
}

/** Replace `from`..`to` with `text`, then select `selStart`..`selEnd` (offsets in the new value). */
export interface Edit {
  from: number
  to: number
  text: string
  selStart: number
  selEnd: number
}

export type Outcome = { kind: 'edit'; edit: Edit } | { kind: 'send' } | undefined

/** A list item's start: its indent, its marker (`-`, `*`, `+`, `1.`, `1)`) and a task box (`[ ]`, `[x]`). */
const ITEM = /^(\s*)([-*+]|\d+[.)])(\s+)(\[[ xX]\]\s+)?/

const lineStart = (v: string, at: number) => v.lastIndexOf('\n', at - 1) + 1
const lineEnd = (v: string, at: number) => {
  const i = v.indexOf('\n', at)
  return i === -1 ? v.length : i
}

/** The marker the next item takes: the same bullet, the next number, an empty task box. */
function nextMarker(m: RegExpMatchArray): string {
  const [, indent, marker, gap, task] = m
  const num = /^(\d+)([.)])$/.exec(marker!)
  const next = num ? `${Number(num[1]) + 1}${num[2]}` : marker
  return `${indent}${next}${gap}${task ? '[ ] ' : ''}`
}

/** Enter: on a list item, the next item (or, on an empty one, the end of the list); `send` when the field sends on Enter. */
function enter(f: Field, sends: boolean): Outcome {
  const ls = lineStart(f.value, f.start)
  const line = f.value.slice(ls, lineEnd(f.value, f.start))
  const m = ITEM.exec(line)
  if (m && f.start === f.end) {
    if (line.trim() === m[0].trim()) {
      // An empty item ends the list: its marker goes, and the line is left empty.
      return { kind: 'edit', edit: { from: ls, to: ls + line.length, text: '', selStart: ls, selEnd: ls } }
    }
    if (f.start >= ls + m[0].length) {
      const text = `\n${nextMarker(m)}`
      return { kind: 'edit', edit: { from: f.start, to: f.end, text, selStart: f.start + text.length, selEnd: f.start + text.length } }
    }
  }
  if (sends) return { kind: 'send' }
  return undefined
}

/**
 * Tab and Shift+Tab: indent or outdent every line the selection touches by two spaces. Only on a list item or a
 * selection: elsewhere the key is left alone, so Tab still moves the focus on (to Send, or the next field).
 */
function indent(f: Field, out: boolean): Outcome {
  const from = lineStart(f.value, f.start)
  if (f.start === f.end && !ITEM.test(f.value.slice(from, lineEnd(f.value, f.start)))) return undefined
  const to = lineEnd(f.value, f.end > f.start && f.value[f.end - 1] === '\n' ? f.end - 1 : f.end)
  const lines = f.value.slice(from, to).split('\n')
  let first = 0
  let total = 0
  const next = lines.map((l, i) => {
    let d: number
    let r: string
    if (out) {
      d = -Math.min(2, l.length - l.trimStart().length)
      r = l.slice(-d)
    } else {
      d = 2
      r = `  ${l}`
    }
    if (i === 0) first = d
    total += d
    return r
  })
  if (out && total === 0) return undefined
  const text = next.join('\n')
  const selStart = Math.max(from, f.start + first)
  const selEnd = f.start === f.end ? selStart : f.end + total
  return { kind: 'edit', edit: { from, to, text, selStart, selEnd } }
}

/** Wraps the selection in `left`/`right` (or unwraps it when it is wrapped so already); with none, puts the cursor between. */
function wrap(f: Field, left: string, right = left): Outcome {
  const sel = f.value.slice(f.start, f.end)
  const before = f.value.slice(f.start - left.length, f.start)
  const after = f.value.slice(f.end, f.end + right.length)
  if (sel && before === left && after === right) {
    return { kind: 'edit', edit: { from: f.start - left.length, to: f.end + right.length, text: sel, selStart: f.start - left.length, selEnd: f.end - left.length } }
  }
  // The selection takes the markers in too (all of `**x**` selected): it is unwrapped the same.
  if (sel.length >= left.length + right.length && sel.startsWith(left) && sel.endsWith(right)) {
    const inner = sel.slice(left.length, sel.length - right.length)
    return { kind: 'edit', edit: { from: f.start, to: f.end, text: inner, selStart: f.start, selEnd: f.start + inner.length } }
  }
  const text = `${left}${sel}${right}`
  return { kind: 'edit', edit: { from: f.start, to: f.end, text, selStart: f.start + left.length, selEnd: f.start + left.length + sel.length } }
}

/** A link: the selection as its text, the cursor where the address goes. */
function link(f: Field): Outcome {
  const sel = f.value.slice(f.start, f.end)
  const text = `[${sel}]()`
  const at = f.start + text.length - 1
  return { kind: 'edit', edit: { from: f.start, to: f.end, text, selStart: sel ? at : f.start + 1, selEnd: sel ? at : f.start + 1 } }
}

/** The letters the field takes with Ctrl (Cmd): bold, italic, code and link. A focused field keeps them from the app's shortcuts. */
export const FIELD_LETTERS = new Set(['B', 'I', 'E', 'K'])

/** What a key does in a markdown field, or undefined to leave it to the field. `sends` is a field that sends on Enter. */
export function composerKey(f: Field, k: KeyPress, sends: boolean): Outcome {
  if (k.alt) return undefined
  if (k.mod && !k.shift) {
    switch (k.key.toUpperCase()) {
      case 'B':
        return wrap(f, '**')
      case 'I':
        return wrap(f, '_')
      case 'E':
        return wrap(f, '`')
      case 'K':
        return link(f)
    }
    return undefined
  }
  if (k.mod) return undefined
  if (k.key === 'Enter' && !k.shift) return enter(f, sends)
  if (k.key === 'Tab') return indent(f, k.shift)
  return undefined
}

/** The field after an edit, for tests and for a field that can't apply an edit natively. */
export function applyEdit(f: Field, e: Edit): Field {
  return { value: f.value.slice(0, e.from) + e.text + f.value.slice(e.to), start: e.selStart, end: e.selEnd }
}
