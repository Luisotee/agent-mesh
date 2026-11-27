/**
 * Files in a prompt: the composer stores each on the agent's host (`AddAttachment`), and the prompt names the path on a line of
 * its own, `[image: <path>]` for an image and `[file: <path>]` for any other file, which the CLI reads with its file tool. A
 * thumbnail of an image is kept on this device only, by path, for the images it sent while open (the host's files are never
 * fetched back). A paste or drop into an agent's terminal is stored the same way and its path typed (`terminalAttachText`).
 */

/** The images the host knows by their bytes, by type, with the extension it wants. */
export const IMAGE_TYPES: Record<string, string> = { 'image/png': '.png', 'image/jpeg': '.jpg', 'image/gif': '.gif', 'image/webp': '.webp' }
export const MAX_ATTACHMENT = 10 << 20

const IMAGE_EXT = /\.(png|jpe?g|gif|webp)$/i

/** Whether a file name or path is an image the host checks the bytes of. */
export const isImagePath = (path: string): boolean => IMAGE_EXT.test(path)

/** Why a file can't go in a prompt, or '' when it can. */
export function attachProblem(f: { size: number }): string {
  if (f.size > MAX_ATTACHMENT) return 'A file is at most 10 MB.'
  if (f.size === 0) return 'That file is empty.'
  return ''
}

/**
 * The name the host is sent: an image's by its type (the host checks the bytes against the extension, so a screenshot named
 * "image" or ".hidden" still gets .png), any other file's as it is.
 */
export function attachName(f: { name: string; type: string }): string {
  const ext = IMAGE_TYPES[f.type]
  if (!ext) return f.name || 'file'
  const base = f.name.replace(/\.[^./\\]*$/, '') || 'image'
  return base + ext
}

/**
 * The files a paste carries as attachments. A clipboard that holds text as well as only images (a spreadsheet or Word selection,
 * a browser's copy of an image with its alt text) is a text paste, so its text is kept; one with a file that is not an image
 * (a file copied in the file manager, which also carries its name as text) is a file paste.
 */
export function pastedFiles(data: DataTransfer | null): File[] {
  const files = [...(data?.files ?? [])]
  if (files.length === 0) return []
  if (data!.getData('text/plain') && files.every((f) => Object.hasOwn(IMAGE_TYPES, f.type.toLowerCase()) || isImagePath(f.name))) return []
  return files
}

export const imageRef = (path: string) => `[image: ${path}]`
export const fileRef = (path: string) => `[file: ${path}]`
/** The prompt line for a stored attachment, by what the path is. */
export const attachRef = (path: string) => (isImagePath(path) ? imageRef(path) : fileRef(path))

const REF = /^\[(?:image|file): (.+)\]$/

/** A prompt's attachment paths (images and other files) and the rest of its text. */
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

/** At most this many thumbnails are kept, and this many characters of them (a data URL of a 10 MB image is 13 MB). */
export const MAX_THUMBS = 40
export const MAX_THUMB_CHARS = 48_000_000

/** Keeps a thumbnail, dropping the oldest past `MAX_THUMBS` or `MAX_THUMB_CHARS` (the new one stays). */
export function keepThumb(path: string, url: string): void {
  thumbs.delete(path)
  thumbs.set(path, url)
  let size = 0
  for (const u of thumbs.values()) size += u.length
  for (const k of [...thumbs.keys()]) {
    if (thumbs.size <= 1 || (thumbs.size <= MAX_THUMBS && size <= MAX_THUMB_CHARS)) break
    size -= thumbs.get(k)!.length
    thumbs.delete(k)
  }
}

/**
 * Why an agent's CLI can't take attachments in a prompt, or '' when it can. Claude Code reads an image or any file with its
 * Read tool and Gemini CLI (agy) with read_file; OpenCode's and Copilot's are not checked yet (TODO.md).
 */
export function imagesUnsupported(kind: string): string {
  return kind === 'claude' || kind === 'agy' ? '' : `Attachments aren't available for ${kind} yet: its CLI reading a file isn't checked.`
}

/**
 * What is typed into an agent's terminal for a file stored on the host: Claude Code takes the path itself (it attaches an
 * image path, and reads any other file when asked), the other CLIs their `@path` mention (Gemini CLI, OpenCode and Copilot all
 * read a file by it). A space in a path is escaped with a backslash, as their prompts take it. A trailing space ends the mention.
 */
export function terminalAttachText(kind: string, path: string): string {
  const p = path.replace(/ /g, '\\ ')
  return (kind === 'claude' ? p : `@${p}`) + ' '
}

/** Why an agent's conversation can't be rewound from the app, or '' when it can (`RewindAgent`: Claude Code only). */
export function rewindUnsupported(kind: string): string {
  return kind === 'claude' ? '' : `Rewind isn't available for ${kind}: use its terminal.`
}
