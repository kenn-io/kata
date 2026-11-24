import { describe, expect, it } from 'vitest'
import { graphOverflowsView } from './viewport'

describe('graphOverflowsView', () => {
  it('detects nodes outside the canvas after pan or zoom', () => {
    const bounds = { x: 10, y: 20, width: 200, height: 100 }
    expect(graphOverflowsView(bounds, { x: 0, y: 0, zoom: 1 }, 300, 200)).toBe(false)
    expect(graphOverflowsView(bounds, { x: -50, y: 0, zoom: 1 }, 300, 200)).toBe(true)
    expect(graphOverflowsView(bounds, { x: 0, y: -50, zoom: 1 }, 300, 200)).toBe(true)
    expect(graphOverflowsView(bounds, { x: 100, y: 0, zoom: 1 }, 300, 200)).toBe(true)
    expect(graphOverflowsView(bounds, { x: 0, y: 100, zoom: 1 }, 300, 200)).toBe(true)
    expect(graphOverflowsView(bounds, { x: 0, y: 0, zoom: 2 }, 300, 200)).toBe(true)
    expect(graphOverflowsView(bounds, { x: 0, y: 0, zoom: 1 }, 0, 0)).toBe(false)
  })
})
