import { describe, expect, it } from 'vitest'
import { MAX_THUMBS, MAX_THUMB_CHARS, attachName, attachProblem, attachRef, fileRef, imageRef, imagesUnsupported, isImagePath, keepThumb, rewindUnsupported, splitImages, terminalAttachText, thumbs } from './attachments'

describe('attachments', () => {
  it('takes a file up to 10 MB, not an empty one', () => {
    expect(attachProblem({ size: 10 })).toBe('')
    expect(attachProblem({ size: 10 << 20 })).toBe('')
    expect(attachProblem({ size: (10 << 20) + 1 })).toMatch(/10 MB/)
    expect(attachProblem({ size: 0 })).toMatch(/empty/)
  })

  it('names an image by its type, as the host checks it, and any other file as it is', () => {
    expect(attachName({ name: 'Screenshot 2026.PNG', type: 'image/png' })).toBe('Screenshot 2026.png')
    expect(attachName({ name: 'image', type: 'image/jpeg' })).toBe('image.jpg')
    expect(attachName({ name: '.hidden', type: 'image/gif' })).toBe('image.gif')
    expect(attachName({ name: 'report.pdf', type: 'application/pdf' })).toBe('report.pdf')
    expect(attachName({ name: 'drawing.svg', type: 'image/svg+xml' })).toBe('drawing.svg')
    expect(attachName({ name: '', type: '' })).toBe('file')
  })

  it('names a stored path by what it is: an image or a file', () => {
    expect(isImagePath('/r/a.PNG')).toBe(true)
    expect(isImagePath('/r/a.jpeg')).toBe(true)
    expect(isImagePath('/r/a.pdf')).toBe(false)
    expect(isImagePath('/r/png')).toBe(false)
    expect(attachRef('/r/a.png')).toBe('[image: /r/a.png]')
    expect(attachRef('/r/a.pdf')).toBe('[file: /r/a.pdf]')
  })

  it('says which CLIs take images and rewind', () => {
    expect(imagesUnsupported('claude')).toBe('')
    expect(imagesUnsupported('agy')).toBe('')
    expect(imagesUnsupported('copilot')).toMatch(/isn't checked/)
    expect(rewindUnsupported('claude')).toBe('')
    expect(rewindUnsupported('agy')).toMatch(/use its terminal/)
  })

  it('keeps the newest thumbnails only', () => {
    thumbs.clear()
    for (let i = 0; i < MAX_THUMBS + 5; i++) keepThumb(`/p${i}`, `data:${i}`)
    expect(thumbs.size).toBe(MAX_THUMBS)
    expect(thumbs.has('/p0')).toBe(false)
    expect(thumbs.get(`/p${MAX_THUMBS + 4}`)).toBe(`data:${MAX_THUMBS + 4}`)
  })

  it('keeps thumbnails under a total size, the newest always', () => {
    thumbs.clear()
    const big = 'x'.repeat(MAX_THUMB_CHARS / 2 - 1)
    keepThumb('/a', big)
    keepThumb('/b', big)
    keepThumb('/c', big)
    expect([...thumbs.keys()]).toEqual(['/b', '/c'])
    keepThumb('/huge', 'x'.repeat(MAX_THUMB_CHARS + 10))
    expect([...thumbs.keys()]).toEqual(['/huge'])
  })
})
