/**
 * What the page may ask Chromium for: the one thing is writing to the clipboard (a copy button, `navigator.clipboard.writeText`
 * on a click). Everything else is refused: the page is ours and loads nothing from outside, so it has no use for the rest.
 * Refusing this one too made every copy button of the app fail ("Write permission denied").
 */
export const allowsPermission = (permission: string): boolean => permission === 'clipboard-sanitized-write'
