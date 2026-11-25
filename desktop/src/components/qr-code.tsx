import QRCode from 'qrcode'
import { useMemo } from 'react'
import { cn } from '@/lib/utils'

/** The dark modules of a QR code as one path of rows: a run of dark modules on a row is one rectangle. */
export function qrPath(text: string): { size: number; path: string } {
  const { modules } = QRCode.create(text, { errorCorrectionLevel: 'M' })
  const size = modules.size
  let path = ''
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      if (!modules.get(y, x)) continue
      let run = 1
      while (x + run < size && modules.get(y, x + run)) run++
      path += `M${x} ${y}h${run}v1h-${run}z`
      x += run - 1
    }
  }
  return { size, path }
}

/**
 * A QR code drawn as SVG by React from the module matrix (no markup string is injected). Black on white with the
 * four-module quiet zone, whatever the theme: scanners need the contrast.
 */
export function QrCode({ text, label, className }: { text: string; label: string; className?: string }) {
  const { size, path } = useMemo(() => qrPath(text), [text])
  return (
    <svg role="img" aria-label={label} viewBox={`-4 -4 ${size + 8} ${size + 8}`} shapeRendering="crispEdges" className={cn('size-56 rounded-md bg-white', className)}>
      <path d={path} className="fill-black" />
    </svg>
  )
}
