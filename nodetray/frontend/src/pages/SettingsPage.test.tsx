import '@testing-library/jest-dom/vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { traymodel } from '../../wailsjs/go/models'
import { SettingsPage, type SettingsPageDependencies } from './SettingsPage'
import { NodeStateProvider } from '../state/NodeStateContext'
import { createTestNodeStore } from '../test/createTestNodeStore'

const ok = { ok: true, errorCode: '', errorSummary: '', uacCancelled: false }

function settings(overrides: Partial<traymodel.TraySettings> = {}): traymodel.TraySettings {
  return new traymodel.TraySettings({
    loginStartTray: false,
    agentStartMode: 'manual',
    helperEnabled: true,
    helperStartMode: 'automatic',
    closeToTray: true,
    refreshIntervalSeconds: 2,
    notificationLevel: 'important',
    ...overrides,
  })
}

function dependencies(overrides: Partial<SettingsPageDependencies> = {}): SettingsPageDependencies {
  return {
    getTraySettings: vi.fn(async () => settings()),
    saveTraySettings: vi.fn(async () => ok),
    openLocation: vi.fn(async () => ok),
    ...overrides,
  }
}

describe('SettingsPage', () => {
  it('结构化呈现完整设置矩阵，Helper 禁用时规范为 manual 后保存', async () => {
    const saveTraySettings = vi.fn(async (value: traymodel.TraySettings) => { void value; return ok })
    const onDirtyChange = vi.fn()
    render(<SettingsPage dependencies={dependencies({ saveTraySettings })} onDirtyChange={onDirtyChange} onRequestExit={() => undefined} />)
    const user = userEvent.setup()

    expect(await screen.findByLabelText('登录后启动托盘程序')).not.toBeChecked()
    expect(screen.getByLabelText('Agent 启动方式')).toHaveValue('manual')
    expect(screen.getByLabelText('启用 Helper')).toBeChecked()
    expect(screen.getByLabelText('Helper 启动方式')).toHaveValue('automatic')
    expect(screen.getByLabelText('关闭窗口时隐藏到托盘')).toBeChecked()
    expect(screen.getByLabelText('状态刷新间隔')).toHaveValue('2')
    expect(screen.getByLabelText('通知级别')).toHaveValue('important')
    expect(screen.getByRole('option', { name: '重要通知' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: '全部通知' })).toBeInTheDocument()

    await user.click(screen.getByLabelText('启用 Helper'))
    expect(screen.getByLabelText('Helper 启动方式')).toBeDisabled()
    await waitFor(() => expect(onDirtyChange).toHaveBeenLastCalledWith(true))
    await user.click(screen.getByRole('button', { name: '保存程序设置' }))
    await waitFor(() => expect(saveTraySettings).toHaveBeenCalledOnce())
    expect(saveTraySettings.mock.calls[0][0]).toMatchObject({ helperEnabled: false, helperStartMode: 'manual' })
    await waitFor(() => expect(onDirtyChange).toHaveBeenLastCalledWith(false))
    expect(document.body).not.toHaveTextContent(/Windows 服务|无人登录/)
  })

  it('保存失败重读实际设置，四个位置按钮只提交固定 LocationKind', async () => {
    const getTraySettings = vi.fn()
      .mockResolvedValueOnce(settings({ refreshIntervalSeconds: 2 }))
      .mockResolvedValueOnce(settings({ refreshIntervalSeconds: 1 }))
    const saveTraySettings = vi.fn(async (value: traymodel.TraySettings) => { void value; return { ...ok, ok: false, errorCode: 'settings_partially_applied' } })
    const openLocation = vi.fn(async (kind: Parameters<SettingsPageDependencies['openLocation']>[0]) => { void kind; return ok })
    const onDirtyChange = vi.fn()
    render(<SettingsPage dependencies={dependencies({ getTraySettings, saveTraySettings, openLocation })} onDirtyChange={onDirtyChange} onRequestExit={() => undefined} />)
    const user = userEvent.setup()
    await screen.findByLabelText('登录后启动托盘程序')

    await user.selectOptions(screen.getByLabelText('状态刷新间隔'), '3')
    await user.click(screen.getByRole('button', { name: '保存程序设置' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('保存失败')
    await waitFor(() => expect(getTraySettings).toHaveBeenCalledTimes(2))
    expect(screen.getByLabelText('状态刷新间隔')).toHaveValue('1')
    await waitFor(() => expect(onDirtyChange).toHaveBeenLastCalledWith(false))

    for (const name of ['打开 Agent 日志', '打开 Helper 日志', '打开 Agent 配置备份', '打开 Helper 配置备份']) {
      await user.click(screen.getByRole('button', { name }))
    }
    expect(openLocation.mock.calls.map(([kind]) => kind)).toEqual([
      'agent-logs', 'helper-logs', 'agent-backup', 'helper-backup',
    ])
    expect(screen.queryByLabelText(/路径|目录/)).not.toBeInTheDocument()
  })

  it('退出按钮只请求统一退出对话框', async () => {
    const onRequestExit = vi.fn()
    render(<SettingsPage dependencies={dependencies()} onDirtyChange={() => undefined} onRequestExit={onRequestExit} />)
    await screen.findByLabelText('登录后启动托盘程序')
    await userEvent.setup().click(screen.getByRole('button', { name: '退出托盘程序' }))
    expect(onRequestExit).toHaveBeenCalledOnce()
  })

  it('启用手动 Helper 后保存、刷新且保持勾选', async () => {
    const saveTraySettings = vi.fn(async (value: traymodel.TraySettings) => {
      void value
      return ok
    })
    const start = vi.fn(async () => undefined)
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <SettingsPage
          dependencies={dependencies({
            getTraySettings: vi.fn(async () => settings({
              helperEnabled: false,
              helperStartMode: 'manual',
            })),
            saveTraySettings,
          })}
          onDirtyChange={() => undefined}
          onRequestExit={() => undefined}
        />
      </NodeStateProvider>,
    )
    const enabled = await screen.findByLabelText('启用 Helper')
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(enabled)
    await userEvent.setup().click(screen.getByRole('button', { name: '保存程序设置' }))

    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
    expect(saveTraySettings).toHaveBeenCalledOnce()
    expect(saveTraySettings.mock.calls[0][0]).toMatchObject({
      helperEnabled: true,
      helperStartMode: 'manual',
    })
    expect(enabled).toBeChecked()
    expect(await screen.findByText('程序设置已保存。')).toBeVisible()
  })

  it('保存成功后等待共享 Overview 刷新再结束 pending', async () => {
    let finishRefresh!: () => void
    const refreshPending = new Promise<void>((resolve) => { finishRefresh = resolve })
    const start = vi.fn()
      .mockResolvedValueOnce(undefined)
      .mockImplementationOnce(() => refreshPending)
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <SettingsPage
          dependencies={dependencies()}
          onDirtyChange={() => undefined}
          onRequestExit={() => undefined}
        />
      </NodeStateProvider>,
    )
    await screen.findByLabelText('登录后启动托盘程序')
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(screen.getByRole('button', { name: '保存程序设置' }))
    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
    expect(screen.getByRole('button', { name: '保存程序设置' })).toBeDisabled()
    expect(screen.queryByText('程序设置已保存。')).not.toBeInTheDocument()

    finishRefresh()
    expect(await screen.findByText('程序设置已保存。')).toBeVisible()
    expect(screen.getByRole('button', { name: '保存程序设置' })).toBeEnabled()
  })
})
