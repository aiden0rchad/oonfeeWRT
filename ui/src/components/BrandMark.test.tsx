import { readFileSync } from 'node:fs'
import { inflateSync } from 'node:zlib'
import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { BrandMark } from './BrandMark'

describe('project orbit mark', () => {
  it('inherits color and stays decorative beside an accessible project name', () => {
    const { container } = render(<BrandMark size={18} className="brand-mark" />)
    const mark = container.querySelector('svg')!
    expect(mark.getAttribute('width')).toBe('18')
    expect(mark.getAttribute('height')).toBe('18')
    expect(mark.getAttribute('class')).toBe('brand-mark')
    expect(mark.getAttribute('stroke')).toBe('currentColor')
    expect(mark.getAttribute('stroke-width')).toBe('1.5')
    expect(mark.getAttribute('aria-hidden')).toBe('true')
    expect(mark.getAttribute('focusable')).toBe('false')
  })

  it('keeps the component, favicon, application master and docs on the same geometry', () => {
    const { container } = render(<BrandMark />)
    const geometry = (element: Element) => Array.from(element.querySelectorAll('path, circle'))
      .map(node => [node.tagName.toLowerCase(), ...Array.from(node.attributes).map(attr => [attr.name, attr.value])])
    const reference = geometry(container)
    for (const path of [
      'public/app-icon.svg', 'public/favicon.svg',
      '../docs/public/logo-light.svg', '../docs/public/logo-dark.svg', '../docs/public/favicon.svg',
    ]) {
      const svg = readFileSync(path, 'utf8')
      const parsed = new DOMParser().parseFromString(svg, 'image/svg+xml')
      expect(geometry(parsed.documentElement), path).toEqual(reference)
      expect(svg, path).toContain('Copyright (c) 2026 Lucide Icons and Contributors')
      expect(svg, path).toContain('ISC License')
    }
    expect(readFileSync('public/favicon.svg', 'utf8')).toBe(readFileSync('../docs/public/favicon.svg', 'utf8'))
  })

  it('ships both correctly sized installed-app PNGs with their icon attribution', () => {
    for (const size of [192, 512]) {
      const png = readFileSync(`public/app-icon-${size}.png`)
      expect(png.subarray(1, 4).toString()).toBe('PNG')
      expect(png.readUInt32BE(16)).toBe(size)
      expect(png.readUInt32BE(20)).toBe(size)
      const comments: string[] = []
      for (let offset = 8; offset < png.length;) {
        const length = png.readUInt32BE(offset)
        const type = png.subarray(offset + 4, offset + 8).toString()
        const data = png.subarray(offset + 8, offset + 8 + length)
        expect(type).not.toBe('tIME')
        if (type === 'tEXt') comments.push(data.toString('latin1'))
        if (type === 'zTXt') comments.push(inflateSync(data.subarray(data.indexOf(0) + 2)).toString())
        offset += length + 12
      }
      expect(comments.join('\n')).toContain('Copyright (c) 2026 Lucide Icons and Contributors')
    }
  })
})
