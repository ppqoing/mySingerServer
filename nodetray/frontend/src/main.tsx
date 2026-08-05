import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { AppErrorBoundary } from './components/AppErrorBoundary'
import './app.css'

const root = document.getElementById('root')
if (root === null) {
  throw new Error('root element unavailable')
}

createRoot(root, {
  onCaughtError: () => undefined,
  onRecoverableError: () => undefined,
}).render(
  <StrictMode>
    <AppErrorBoundary>
      <App />
    </AppErrorBoundary>
  </StrictMode>,
)
