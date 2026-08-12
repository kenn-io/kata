import { describe, expect, test } from 'vitest'

import {
  detectInstallPlatform,
  initializeInstallPanel,
} from '../../../../docs/javascripts/install-platform.js'

describe('detectInstallPlatform', () => {
  test.each([
    ['macOS', 'macos'],
    ['MacIntel', 'macos'],
    ['iPhone', 'macos'],
    ['iOS', 'macos'],
    ['Win32', 'windows'],
    ['Windows', 'windows'],
    ['Linux x86_64', 'linux'],
    ['X11', 'linux'],
    ['Android', 'linux'],
    ['', 'macos'],
    [undefined, 'macos'],
    ['Plan 9', 'macos'],
  ])('maps %s to %s', (platform, expected) => {
    expect(detectInstallPlatform(platform)).toBe(expected)
  })
})

function renderPanel() {
  document.body.innerHTML = `
    <section data-install-panel>
      <button data-install-platform-button="macos" aria-pressed="true">macOS</button>
      <button data-install-platform-button="linux" aria-pressed="false">Linux</button>
      <button data-install-platform-button="windows" aria-pressed="false">Windows</button>
      <div data-install-platform-content="macos">brew</div>
      <div class="kata-install-command--fallback-hidden" data-install-platform-content="linux">curl</div>
      <div class="kata-install-command--fallback-hidden" data-install-platform-content="windows">irm</div>
      <p data-install-platform-status>macOS selected · choose another platform anytime.</p>
    </section>`
}

function button(platform) {
  return document.querySelector(`[data-install-platform-button="${platform}"]`)
}

function content(platform) {
  return document.querySelector(`[data-install-platform-content="${platform}"]`)
}

describe('initializeInstallPanel', () => {
  test('applies the detected platform to accessible DOM state', () => {
    renderPanel()
    initializeInstallPanel(document, 'Win32')

    expect(button('windows')?.getAttribute('aria-pressed')).toBe('true')
    expect(button('macos')?.getAttribute('aria-pressed')).toBe('false')
    expect(content('windows')?.hidden).toBe(false)
    expect(content('macos')?.hidden).toBe(true)
    expect(content('windows')?.classList).not.toContain('kata-install-command--fallback-hidden')
    expect(document.querySelector('[data-install-platform-status]')?.textContent).toBe(
      'Windows selected · choose another platform anytime.',
    )
  })

  test('manual choice lasts until a fresh initialization', () => {
    renderPanel()
    initializeInstallPanel(document, 'MacIntel')

    button('linux')?.click()
    expect(button('linux')?.getAttribute('aria-pressed')).toBe('true')
    expect(content('linux')?.hidden).toBe(false)

    initializeInstallPanel(document, 'MacIntel')
    expect(button('macos')?.getAttribute('aria-pressed')).toBe('true')
    expect(content('macos')?.hidden).toBe(false)
    expect(content('linux')?.hidden).toBe(true)
  })

  test('does nothing when the homepage panel is absent', () => {
    document.body.replaceChildren()
    expect(() => initializeInstallPanel(document, 'Linux')).not.toThrow()
  })
})
