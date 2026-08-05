import '@testing-library/jest-dom/vitest'
import { render, screen, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { AppErrorBoundary } from './AppErrorBoundary'

const componentCalls = vi.hoisted(() => ({
  startAgent: vi.fn(),
  stopAgent: vi.fn(),
  restartAgent: vi.fn(),
  forceStopAgent: vi.fn(),
  startHelper: vi.fn(),
  stopHelper: vi.fn(),
  restartHelper: vi.fn(),
  forceStopHelper: vi.fn(),
  exitTray: vi.fn(),
}))

vi.mock('../../wailsjs/go/main/Backend', () => ({
  StartAgent: componentCalls.startAgent,
  StopAgent: componentCalls.stopAgent,
  RestartAgent: componentCalls.restartAgent,
  ForceStopAgent: componentCalls.forceStopAgent,
  StartHelper: componentCalls.startHelper,
  StopHelper: componentCalls.stopHelper,
  RestartHelper: componentCalls.restartHelper,
  ForceStopHelper: componentCalls.forceStopHelper,
}))

function renderWithReactErrorCallbacks(ui: ReactNode) {
  const consoleError = vi.spyOn(console, 'error').mockImplementation(() => undefined)
  try {
    return render(ui, {
      onCaughtError: () => undefined,
      onRecoverableError: () => undefined,
    })
  } finally {
    consoleError.mockRestore()
  }
}

describe('AppErrorBoundary', () => {
  it('正常渲染时不改变子树', () => {
    render(
      <AppErrorBoundary>
        <p>节点控制台正常</p>
      </AppErrorBoundary>,
    )

    expect(screen.getByText('节点控制台正常')).toBeVisible()
  })

  it('第一次渲染失败只自动重建子树一次并恢复', async () => {
    let renders = 0
    const Recoverable = (): ReactNode => {
      renders += 1
      if (renders <= 2) {
        throw new Error('password=fixture-secret C:\\private\\agent.json')
      }
      return <p>界面已恢复</p>
    }

    renderWithReactErrorCallbacks(
      <AppErrorBoundary>
        <Recoverable />
      </AppErrorBoundary>,
    )

    expect(await screen.findByText('界面已恢复')).toBeVisible()
    expect(renders).toBe(3)
    expect(screen.queryByText(/fixture-secret|private|agent\.json/i)).not.toBeInTheDocument()
  })

  it('第二次失败显示稳定中文界面且不再循环重建', async () => {
    let renders = 0
    const Broken = (): ReactNode => {
      renders += 1
      throw new Error('postgresql://user:secret@private/db')
    }

    renderWithReactErrorCallbacks(
      <AppErrorBoundary>
        <Broken />
      </AppErrorBoundary>,
    )

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('请重启托盘程序')
    expect(alert).not.toHaveTextContent(/postgres|secret|private|stack/i)
    const stableRenderCount = renders
    await waitFor(() => expect(renders).toBe(stableRenderCount))
    expect(stableRenderCount).toBeGreaterThanOrEqual(2)
  })

  it('恢复和失败路径不调用组件控制或退出绑定', async () => {
    const Broken = (): ReactNode => {
      throw new Error('private fixture')
    }

    renderWithReactErrorCallbacks(
      <AppErrorBoundary>
        <Broken />
      </AppErrorBoundary>,
    )
    expect(await screen.findByRole('alert')).toBeVisible()

    for (const operation of Object.values(componentCalls)) {
      expect(operation).not.toHaveBeenCalled()
    }
  })
})
