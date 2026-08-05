import '@testing-library/jest-dom/vitest'
import { act, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { App } from './App'
import { EventsOn } from '../wailsjs/runtime/runtime'

const backendMocks = vi.hoisted(() => {
  const ok = { ok: true, errorCode: '', errorSummary: '', uacCancelled: false }
  return {
    ok,
    getTraySettings: vi.fn(async () => ({
      loginStartTray: false,
      agentStartMode: 'manual',
      helperEnabled: false,
      helperStartMode: 'manual',
      closeToTray: true,
      refreshIntervalSeconds: 2,
      notificationLevel: 'important',
    })),
    saveTraySettings: vi.fn(async () => ok),
    exitTray: vi.fn(async () => ok),
    forceStopAgent: vi.fn(async () => ok),
    forceStopHelper: vi.fn(async () => ok),
    forceExitAll: vi.fn(async () => ({ ok: true, failedComponents: [], errorCode: '', errorSummary: '' })),
  }
})

vi.mock('../wailsjs/go/main/Backend', () => ({
  GetOverview: vi.fn(async () => { throw new Error('offline fixture') }),
  GetAgentForm: vi.fn(async () => { throw new Error('offline fixture') }),
  ValidateAgent: vi.fn(async () => []), SaveAgent: vi.fn(async () => backendMocks.ok), SaveAndRestartAgent: vi.fn(async () => backendMocks.ok),
  StartAgent: vi.fn(async () => backendMocks.ok), StopAgent: vi.fn(async () => backendMocks.ok), RestartAgent: vi.fn(async () => backendMocks.ok), ForceStopAgent: backendMocks.forceStopAgent,
  GetHelperForm: vi.fn(async () => { throw new Error('offline fixture') }),
  ValidateHelper: vi.fn(async () => []), SaveHelper: vi.fn(async () => backendMocks.ok),
  StartHelper: vi.fn(async () => backendMocks.ok), StopHelper: vi.fn(async () => backendMocks.ok), RestartHelper: vi.fn(async () => backendMocks.ok), ForceStopHelper: backendMocks.forceStopHelper,
  GetTraySettings: backendMocks.getTraySettings, SaveTraySettings: backendMocks.saveTraySettings,
  OpenLocation: vi.fn(async () => backendMocks.ok),
  ForceExitAll: backendMocks.forceExitAll,
}))

vi.mock('../wailsjs/runtime/runtime', () => ({
  EventsOn: vi.fn(() => () => undefined),
}))

function selectedTabName(): string | null {
  return screen.getAllByRole('tab').find((tab) => tab.getAttribute('aria-selected') === 'true')?.textContent ?? null
}

describe('App shell', () => {
  beforeEach(() => {
    window.location.hash = ''
    vi.mocked(EventsOn).mockClear()
  })

  it('生产入口同时订阅窗口关闭与托盘强制退出请求', () => {
    render(<App />)

    expect(vi.mocked(EventsOn).mock.calls.map(([name]) => name)).toEqual([
      'window-close-requested',
      'force-exit-requested',
    ])
  })

  it('renders the fixed four tabs with overview selected and associated panels', () => {
    render(<App />)

    expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual([
      '总览',
      'Agent',
      '删除 Helper',
      '程序设置',
    ])
    expect(selectedTabName()).toBe('总览')

    const panels = screen.getAllByRole('tabpanel', { hidden: true })
    expect(panels).toHaveLength(4)
    for (const tab of screen.getAllByRole('tab')) {
      const panel = panels.find((candidate) => candidate.id === tab.getAttribute('aria-controls'))
      expect(panel).toHaveAttribute('aria-labelledby', tab.id)
    }
  })

  it('supports click, wrapping arrow keys, Home and End', async () => {
    const user = userEvent.setup()
    render(<App />)

    const tabs = screen.getAllByRole('tab')
    await user.click(tabs[1])
    expect(selectedTabName()).toBe('Agent')
    expect(tabs[1]).toHaveFocus()

    await user.keyboard('{ArrowRight}')
    expect(selectedTabName()).toBe('删除 Helper')
    expect(tabs[2]).toHaveFocus()

    await user.keyboard('{End}')
    expect(selectedTabName()).toBe('程序设置')
    await user.keyboard('{ArrowRight}')
    expect(selectedTabName()).toBe('总览')

    await user.keyboard('{ArrowLeft}')
    expect(selectedTabName()).toBe('程序设置')
    await user.keyboard('{Home}')
    expect(selectedTabName()).toBe('总览')
  })

  it('falls back to overview for an unknown local hash', () => {
    window.location.hash = '#/not-supported'
    render(<App />)

    expect(selectedTabName()).toBe('总览')
    expect(screen.getByRole('tabpanel', { name: '总览' })).toBeVisible()
    expect(screen.queryByText('not-supported')).not.toBeInTheDocument()
  })

  it('keeps selection and focus aligned for direct key events', () => {
    render(<App />)
    const first = screen.getByRole('tab', { name: '总览' })
    first.focus()
    fireEvent.keyDown(first, { key: 'ArrowLeft' })
    expect(screen.getByRole('tab', { name: '程序设置' })).toHaveFocus()
    expect(selectedTabName()).toBe('程序设置')
  })

  it('只订阅一次原生关闭事件，卸载时取消，并打开与设置页共用的退出对话框', async () => {
    let requestClose: (() => void) | undefined
    const unsubscribe = vi.fn()
    const subscribeWindowClose = vi.fn((handler: () => void) => {
      requestClose = handler
      return unsubscribe
    })
    const { unmount } = render(<App subscribeWindowClose={subscribeWindowClose} />)
    expect(subscribeWindowClose).toHaveBeenCalledOnce()

    act(() => requestClose?.())
    expect(await screen.findByRole('dialog', { name: '确认强制退出' })).toBeVisible()
    unmount()
    expect(unsubscribe).toHaveBeenCalledOnce()
  })

  it('页签隐藏不卸载 dirty 草稿，原生关闭时明确提示未保存修改', async () => {
    let requestClose: (() => void) | undefined
    const user = userEvent.setup()
    render(<App subscribeWindowClose={(handler) => { requestClose = handler; return () => undefined }} />)

    await user.click(screen.getByRole('tab', { name: '程序设置' }))
    const loginStart = await screen.findByLabelText('登录后启动托盘程序')
    await user.click(loginStart)
    expect(loginStart).toBeChecked()
    await user.click(screen.getByRole('tab', { name: '总览' }))
    await user.click(screen.getByRole('tab', { name: '程序设置' }))
    expect(screen.getByLabelText('登录后启动托盘程序')).toBeChecked()

    act(() => requestClose?.())
    expect(await screen.findByText(/未保存修改/)).toBeVisible()
  })
})
