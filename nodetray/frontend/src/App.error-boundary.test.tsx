import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import { App } from './App'
import { AppErrorBoundary } from './components/AppErrorBoundary'

const pageMocks = vi.hoisted(() => ({
  renderOverview: vi.fn(() => '总览已恢复'),
}))

vi.mock('./pages/OverviewPage', () => ({
  OverviewPage: () => pageMocks.renderOverview(),
}))

vi.mock('./pages/AgentPage', () => ({ AgentPage: () => null }))
vi.mock('./pages/HelperPage', () => ({ HelperPage: () => null }))
vi.mock('./pages/SettingsPage', () => ({ SettingsPage: () => null }))

vi.mock('../wailsjs/runtime/runtime', () => ({
  EventsOn: vi.fn(() => () => undefined),
}))

describe('App root error boundary wiring', () => {
  beforeEach(() => {
    window.location.hash = ''
    pageMocks.renderOverview.mockReset()
  })

  it('lets a real page render error reach the root boundary and rebuilds once', async () => {
    let attempts = 0
    pageMocks.renderOverview.mockImplementation(() => {
      attempts += 1
      if (attempts <= 2) {
        throw new Error('password=secret')
      }
      return '总览已恢复'
    })

    render(
      <AppErrorBoundary>
        <App />
      </AppErrorBoundary>,
      { onCaughtError: () => undefined, onRecoverableError: () => undefined },
    )

    expect(await screen.findByText('总览已恢复')).toBeVisible()
    expect(screen.queryByText(/secret/i)).not.toBeInTheDocument()
  })

  it('uses the stable root fallback when the rebuilt App also fails', async () => {
    pageMocks.renderOverview.mockImplementation(() => {
      throw new Error('password=secret')
    })

    render(
      <AppErrorBoundary>
        <App />
      </AppErrorBoundary>,
      { onCaughtError: () => undefined, onRecoverableError: () => undefined },
    )

    expect(await screen.findByRole('alert')).toHaveTextContent('请重启托盘程序')
    expect(screen.queryByText(/secret/i)).not.toBeInTheDocument()
  })
})
