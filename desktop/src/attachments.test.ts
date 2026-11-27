import { describe, expect, it } from 'vitest'
import { imageName, imageProblem, imageRef, imagesUnsupported, rewindUnsupported, splitImages } from './attachments'

describe('attachments', () => {
  it('takes PNG, JPEG, GIF and WebP up to 10 MB', () => {
    expect(imageProblem({ type: 'image/png', size: 10 })).toBe('')
    expect(imageProblem({ type: 'image/webp', size: 10 << 20 })).toBe('')
    expect(imageProblem({ type: 'image/svg+xml', size: 10 })).toMatch(/Only PNG/)
    expect(imageProblem({ type: 'image/png', size: (10 << 20) + 1 })).toMatch(/10 MB/)
    expect(imageProblem({ type: 'image/png', size: 0 })).toMatch(/empty/)
  })

  it('names a file by its type, as the host checks it', () => {
    expect(imageName({ name: 'Screenshot 2026.PNG', type: 'image/png' })).toBe('Screenshot 2026.png')
    expect(imageName({ name: 'image', type: 'image/jpeg' })).toBe('image.jpg')
    expect(imageName({ name: '.hidden', type: 'image/gif' })).toBe('image.gif')
  })

  it('reads the image paths back out of a prompt', () => {
    const text = [imageRef('/r/a.png'), imageRef('/r/b c.jpg'), 'look at these', 'and [image: not alone] here'].join('\n')
    expect(splitImages(text)).toEqual({ paths: ['/r/a.png', '/r/b c.jpg'], text: 'look at these\nand [image: not alone] here' })
    expect(splitImages('plain')).toEqual({ paths: [], text: 'plain' })
  })

  it('says which CLIs take images and rewind', () => {
    expect(imagesUnsupported('claude')).toBe('')
    expect(imagesUnsupported('agy')).toBe('')
    expect(imagesUnsupported('copilot')).toMatch(/isn't checked/)
    expect(rewindUnsupported('claude')).toBe('')
    expect(rewindUnsupported('agy')).toMatch(/use its terminal/)
  })
})
