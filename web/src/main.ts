import { mount } from 'svelte'

import App from './App.svelte'
import './app.css'
import { installPreloadRecovery } from './lib/state/preloadRecovery'

installPreloadRecovery({
  target: window,
  storage: window.sessionStorage,
  entrypoint: new URL(import.meta.url).pathname,
  reload: () => window.location.reload(),
  showMismatch: () => window.dispatchEvent(new Event('kata:versionMismatch')),
})

const target = document.getElementById('app')

if (!target) {
  throw new Error("Root element 'app' not found")
}

mount(App, { target })
