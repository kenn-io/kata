const DEFAULT_PLATFORM = 'macos'

const PLATFORM_LABELS = {
  macos: 'macOS',
  linux: 'Linux',
  windows: 'Windows',
}

const initializedPanels = new WeakSet()

export function detectInstallPlatform(platform) {
  const value = typeof platform === 'string' ? platform.toLowerCase() : ''

  if (value.includes('win')) return 'windows'
  if (value.includes('mac') || value.includes('iphone') || value.includes('ipad')) {
    return 'macos'
  }
  if (value.includes('linux') || value.includes('x11') || value.includes('android')) {
    return 'linux'
  }
  return DEFAULT_PLATFORM
}

function selectInstallPlatform(panel, platform) {
  for (const button of panel.querySelectorAll('[data-install-platform-button]')) {
    button.setAttribute(
      'aria-pressed',
      String(button.dataset.installPlatformButton === platform),
    )
  }

  for (const content of panel.querySelectorAll('[data-install-platform-content]')) {
    content.hidden = content.dataset.installPlatformContent !== platform
  }

  const status = panel.querySelector('[data-install-platform-status]')
  if (status) {
    status.textContent = `${PLATFORM_LABELS[platform]} selected · choose another platform anytime.`
  }
}

function readBrowserPlatform() {
  return navigator.userAgentData?.platform || navigator.platform
}

export function initializeInstallPanel(root, browserPlatform = readBrowserPlatform()) {
  const panel = root.querySelector('[data-install-panel]')
  if (!panel) return

  if (!initializedPanels.has(panel)) {
    panel.addEventListener('click', (event) => {
      if (!(event.target instanceof Element)) return
      const button = event.target.closest('[data-install-platform-button]')
      if (button && panel.contains(button)) {
        selectInstallPlatform(panel, button.dataset.installPlatformButton)
      }
    })
    initializedPanels.add(panel)
  }

  selectInstallPlatform(panel, detectInstallPlatform(browserPlatform))
}

function initializeCurrentPage() {
  initializeInstallPanel(document)
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initializeCurrentPage, { once: true })
} else {
  initializeCurrentPage()
}

globalThis.document$?.subscribe(initializeCurrentPage)
