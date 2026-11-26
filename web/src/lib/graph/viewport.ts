import type { Rect, Viewport } from '@xyflow/svelte'

export function graphOverflowsView(
  bounds: Rect,
  viewport: Viewport,
  width: number,
  height: number,
): boolean {
  if (width <= 0 || height <= 0 || bounds.width <= 0 || bounds.height <= 0) return false
  const left = bounds.x * viewport.zoom + viewport.x
  const top = bounds.y * viewport.zoom + viewport.y
  const right = left + bounds.width * viewport.zoom
  const bottom = top + bounds.height * viewport.zoom
  return left < -1 || top < -1 || right > width + 1 || bottom > height + 1
}
