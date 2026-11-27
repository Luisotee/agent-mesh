import { MARKS } from '@/vendor-marks'
import { VENDOR_NAME, vendorOf, type Vendor } from '@/vendor'
import { cn } from '@/lib/utils'

/** Each vendor's brand colour, a theme token per theme (the monochrome brands use the theme's text colour). */
const COLOR: Record<Vendor, string> = {
  claude: 'text-vendor-claude',
  gemini: 'text-vendor-gemini',
  openai: 'text-vendor-openai',
  copilot: 'text-vendor-copilot',
  opencode: 'text-vendor-opencode',
  deepseek: 'text-vendor-deepseek',
  other: 'text-muted-foreground',
}

/**
 * The mark of the vendor behind a CLI or a model (simple-icons, CC0, in the vendor's brand colour): by the agent's CLI
 * `kind` and `model`, or by a `vendor` given. OpenAI has no mark in the set and an unknown kind has none at all: both
 * get a letter badge, so a row never lacks its icon.
 */
export function VendorIcon({ kind = '', model = '', vendor, className }: { kind?: string; model?: string; vendor?: Vendor; className?: string }) {
  const v = vendor ?? vendorOf(kind, model)
  const mark = MARKS[v === 'claude' || v === 'gemini' || v === 'copilot' || v === 'opencode' || v === 'deepseek' ? v : '']
  const label = v === 'other' && kind ? kind : VENDOR_NAME[v]
  if (mark) {
    return (
      <svg role="img" aria-label={label} viewBox="0 0 24 24" fill="currentColor" className={cn('size-3.5 shrink-0', COLOR[v], className)}>
        <title>{label}</title>
        <path d={mark} />
      </svg>
    )
  }
  const letter = v === 'openai' ? 'O' : (kind.trim()[0] ?? '?').toUpperCase()
  return (
    <span role="img" aria-label={label} title={label} className={cn('inline-grid size-3.5 shrink-0 place-items-center rounded-sm border border-current text-[9px] leading-none font-bold', COLOR[v], className)}>
      {letter}
    </span>
  )
}
