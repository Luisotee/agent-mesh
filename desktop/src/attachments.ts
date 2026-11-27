/**
 * Images in a prompt: the composer stores each on the agent's host (`AddAttachment`), and the prompt names the path
 * as `[image: <path>]` on a line of its own, which the CLI reads with its file tool. A thumbnail is kept on this
 * device only, by path, for the images it sent while open (the host's files are never fetched back).
 */

/** The images a prompt may carry, by type, with the extension the host wants. */
export const IMAGE_TYPES: Record<string, string> = { 'image/png': '.png', 'image/jpeg': '.jpg', 'image/gif': '.gif', 'image/webp': '.webp' }
export const MAX_IMAGE = 10 << 20

/** Why a file can't go in a prompt, or '' when it can. */
export function imageProblem(f: { type: string; size: number }): string {
  if (!IMAGE_TYPES[f.type]) return 'Only PNG, JPEG, GIF and WebP images can go in a prompt.'
  if (f.size > MAX_IMAGE) return 'An image is at most 10 MB.'
  if (f.size === 0) return 'That image is empty.'
  return ''
}

/** The file's name with the extension its type has, for the host's check. */
export function imageName(f: { name: string; type: string }): string {
  const ext = IMAGE_TYPES[f.type] ?? ''
  const base = f.name.replace(/\.[^./\\]*$/, '') || 'image'
  return base + ext
}

export const imageRef = (path: string) => `[image: ${path}]`

const REF = /^\[image: (.+)\]$/

/** A prompt's image paths and the rest of its text. */
export function splitImages(text: string): { paths: string[]; text: string } {
  const paths: string[] = []
  const rest = text.split('\n').filter((l) => {
    const m = REF.exec(l.trim())
    if (m) paths.push(m[1]!)
    return !m
  })
  return { paths, text: rest.join('\n').replace(/^\s+/, '') }
}

/** The thumbnails of the images this device sent, by their host path (data URLs: the page's CSP allows them). */
export const thumbs = new Map<string, string>()

/** At most this many thumbnails are kept, the oldest dropped first (each may be megabytes). */
export const MAX_THUMBS = 40

/** Keeps a thumbnail, dropping the oldest past `MAX_THUMBS`. */
export function keepThumb(path: string, url: string): void {
  thumbs.delete(path)
  thumbs.set(path, url)
  while (thumbs.size > MAX_THUMBS) thumbs.delete(thumbs.keys().next().value!)
}

/**
 * Why an agent's CLI can't take images, or '' when it can. Claude Code reads an image file with its Read tool and
 * Gemini CLI (agy) with read_file; OpenCode's and Copilot's are not checked yet (TODO.md).
 */
export function imagesUnsupported(kind: string): string {
  return kind === 'claude' || kind === 'agy' ? '' : `Images aren't available for ${kind} yet: its CLI reading an image file isn't checked.`
}

/** Why an agent's conversation can't be rewound from the app, or '' when it can (`RewindAgent`: Claude Code only). */
export function rewindUnsupported(kind: string): string {
  return kind === 'claude' ? '' : `Rewind isn't available for ${kind}: use its terminal.`
}
