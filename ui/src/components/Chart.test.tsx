import { render } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type uPlot from 'uplot'
import { TimeChart } from './Chart'

const drawn = vi.hoisted(() => ({ options: null as uPlot.Options | null, data: null as uPlot.AlignedData | null }))
vi.mock('uplot', () => ({
  default: class {
    constructor(options: uPlot.Options, data: uPlot.AlignedData) {
      drawn.options = options
      drawn.data = data
    }
    setSize() {}
    destroy() {}
  },
}))

function draw(values: Array<number | null>) {
  render(<TimeChart
    label="Traffic"
    colour="#3366aa"
    window={[0, 300 * values.length]}
    format={String}
    points={values.map((avg, index) => ({ ts: index * 300, avg, min: avg, max: avg, cnt: avg == null ? 0 : 1 }))}
  />)
  return drawn.options!
}

describe('TimeChart presentation', () => {
  beforeEach(() => { drawn.options = null; drawn.data = null })

  it('draws connected observations as a line, even when few samples fill a small part of the window', () => {
    const values = [...Array<number | null>(270).fill(null), ...Array.from({ length: 17 }, (_, i) => i)]
    const options = draw(values)
    expect(options.series[3].points).toMatchObject({ show: false, filter: null })
    expect(options.series[3].spanGaps).toBe(false)
    expect(drawn.data?.[3]).toEqual(values)
    expect(options.axes?.[0].grid?.show).toBe(false)
    expect(options.cursor).toMatchObject({ points: { size: 5 } })
  })

  it('keeps isolated samples visible without adding markers to connected runs', () => {
    const options = draw([2, null, 3, 4, null, 0])
    expect(options.series[3].points).toMatchObject({ show: false, filter: [0, 5], size: 4, fill: '#3366aa' })
    expect(drawn.data?.[3]).toEqual([2, null, 3, 4, null, 0])
  })

  it('preserves a lone measured zero and its min/max band', () => {
    const options = draw([null, 0, null])
    expect(options.series[3].points?.filter).toEqual([1])
    expect(options.bands).toEqual([{ series: [1, 2], fill: 'rgba(51, 102, 170, 0.1)' }])
    expect(drawn.data?.[1]).toEqual([null, 0, null])
    expect(drawn.data?.[2]).toEqual([null, 0, null])
  })

  it('sizes the Y gutter from formatted tick widths without changing data or scales', () => {
    const options = draw([200_000, 210_000])
    const size = options.axes?.[1].size
    if (typeof size !== 'function') throw new Error('Y axis must size itself from its labels')
    const ctx = {
      font: '22px unrelated-canvas-font', save: vi.fn(), restore: vi.fn(),
      measureText: vi.fn((value: string) => ({ width: value === '200 kB/s' ? 94.2 : 20 })),
    }
    const plot = { ctx } as unknown as uPlot
    expect(size(plot, null as unknown as string[], 1, 0)).toBe(58)
    expect(size(plot, ['0 B/s', '200 kB/s'], 1, 1)).toBe(114)
    expect(ctx.font).toBe('11px ui-sans-serif, system-ui, sans-serif')
    expect(ctx.save).toHaveBeenCalledOnce()
    expect(ctx.restore).toHaveBeenCalledOnce()
    expect(size(plot, ['0 B/s'], 1, 2)).toBe(58)
    expect(options.axes?.[1]).toMatchObject({ gap: 5, ticks: { size: 10 } })
    expect(options.scales?.y).toEqual({})
    expect(drawn.data?.[3]).toEqual([200_000, 210_000])
  })
})
