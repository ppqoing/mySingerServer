import '@testing-library/jest-dom/vitest'
import { cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { vi } from 'vitest'
import { config, traymodel } from '../../wailsjs/go/models'
import { NodeStateProvider } from '../state/NodeStateContext'
import type { NodeOverview } from '../state/nodeStore'
import { createTestNodeStore } from '../test/createTestNodeStore'
import { HelperPage, type HelperPageDependencies } from './HelperPage'
import { SettingsPage, type SettingsPageDependencies } from './SettingsPage'

const ok = { ok: true, errorCode: '', errorSummary: '', uacCancelled: false }
const configOK = {
  ok: true, saved: true, restarted: false, sha256: 'd'.repeat(64), needsRestart: false,
  errorCode: '', errorSummary: '',
}

function helperForm(overrides: Partial<config.HelperForm> = {}): config.HelperForm {
  return new config.HelperForm({
    pipeName: 'helper-pipe',
    allowedRoots: ['C:\\fixtures\\media-a'],
    deniedRoots: ['C:\\fixtures\\media-a\\private'],
    defaultMode: 'soft',
    allowHardDelete: false,
    recycleDirName: '.recycle',
    maxEntriesPerFrame: 512,
    frameReadTimeoutSec: 20,
    frameWriteTimeoutSec: 20,
    logDir: 'C:\\fixtures\\helper-logs',
    ...overrides,
  })
}

function component(lifecycle = 'stopped') {
  return {
    lifecycle,
    healthy: lifecycle === 'running',
    ready: lifecycle === 'running',
    pid: lifecycle === 'running' ? 3101 : 0,
    startedAtUnixMs: lifecycle === 'running' ? 1000 : 0,
    uptimeSeconds: 0,
    workerReady: 0,
    workerExpected: 0,
    activeRequests: 0,
    errorCode: '',
    errorSummary: '',
    needsAttention: false,
    runtimeConfigSha256: '',
    savedConfigSha256: '',
    needsRestart: false,
  }
}

function overview(overrides: Partial<NodeOverview> = {}): NodeOverview {
  return {
    machineId: 'node-a',
    agent: component('running'),
    workers: [],
    helper: component('stopped'),
    agentStartMode: 'automatic',
    helperStartMode: 'manual',
    helperEnabled: true,
    helperTaskDrift: false,
    loginStartDrift: false,
    ...overrides,
  }
}

function traySettings(overrides: Partial<traymodel.TraySettings> = {}): traymodel.TraySettings {
  return new traymodel.TraySettings({
    loginStartTray: false,
    agentStartMode: 'manual',
    helperEnabled: false,
    helperStartMode: 'manual',
    closeToTray: true,
    refreshIntervalSeconds: 2,
    notificationLevel: 'important',
    ...overrides,
  })
}

function settingsDependencies(overrides: Partial<SettingsPageDependencies> = {}): SettingsPageDependencies {
  return {
    getTraySettings: async () => traySettings(),
    saveTraySettings: async () => ok,
    openLocation: async () => ok,
    ...overrides,
  }
}

function dependencies(overrides: Partial<HelperPageDependencies> = {}): HelperPageDependencies {
  return {
    getHelperForm: async () => helperForm(),
    getOverview: async () => overview(),
    validateHelper: async () => [],
    saveHelper: async () => configOK,
    startHelper: async () => ok,
    stopHelper: async () => ok,
    restartHelper: async () => ok,
    forceStopHelper: async () => ok,
    confirmHardDelete: async () => true,
    confirmForceStop: async () => true,
    ...overrides,
  }
}

async function renderPage(deps = dependencies()) {
  render(<HelperPage dependencies={deps} />)
  await screen.findByRole('heading', { name: '删除 Helper 配置' })
  return deps
}

describe('HelperPage', () => {
  it('只在表单 dirty 状态变化时向壳上报', async () => {
    const onDirtyChange = vi.fn()
    render(<HelperPage dependencies={dependencies()} onDirtyChange={onDirtyChange} />)
    const pipe = await screen.findByLabelText('管道名称')
    await waitFor(() => expect(onDirtyChange).toHaveBeenLastCalledWith(false))
    await userEvent.setup().type(pipe, '-changed')
    await waitFor(() => expect(onDirtyChange).toHaveBeenLastCalledWith(true))
  })

  it('初次读取表单和总览，按合同显示全部结构化字段与白黑名单', async () => {
    const getHelperForm = vi.fn(async () => helperForm())
    const getOverview = vi.fn(async () => overview())
    await renderPage(dependencies({ getHelperForm, getOverview }))

    expect(getHelperForm).toHaveBeenCalledOnce()
    expect(getOverview).toHaveBeenCalledOnce()
    for (const label of [
      '管道名称', '允许的媒体根目录', '拒绝的媒体根目录', '回收目录名称',
      '默认删除模式', '允许硬删除', '每帧最大条目', '读取超时', '写入超时', '日志目录',
    ]) expect(screen.getByLabelText(label)).toBeInTheDocument()
    expect(screen.getByText('C:\\fixtures\\media-a')).toBeVisible()
    expect(screen.getByText('C:\\fixtures\\media-a\\private')).toBeVisible()
    expect(screen.getByRole('button', { name: '添加允许的媒体根目录' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '添加拒绝的媒体根目录' })).toBeEnabled()
  })

  it('校验错误按 FieldError.Field 显示到列表和条目且不保存', async () => {
    const saveHelper = vi.fn(async () => configOK)
    await renderPage(dependencies({
      saveHelper,
      validateHelper: async () => [
        new config.FieldError({ field: 'allowedRoots', code: 'required', message: '至少配置一个允许根目录' }),
        new config.FieldError({ field: 'deniedRoots[0]', code: 'overlap', message: '该目录与允许根目录重叠' }),
      ],
    }))
    await userEvent.setup().click(screen.getByRole('button', { name: '保存 Helper 配置' }))

    expect(await screen.findByText('至少配置一个允许根目录')).toBeVisible()
    expect(screen.getByText('该目录与允许根目录重叠')).toBeVisible()
    expect(saveHelper).not.toHaveBeenCalled()
  })

  it('关闭硬删除直接保存；加载已有 true 保持选中并持续警告，取消确认绝不保存', async () => {
    const saveFalse = vi.fn(async () => configOK)
    const confirmFalse = vi.fn(async () => true)
    await renderPage(dependencies({ saveHelper: saveFalse, confirmHardDelete: confirmFalse }))
    await userEvent.setup().click(screen.getByRole('button', { name: '保存 Helper 配置' }))
    await waitFor(() => expect(saveFalse).toHaveBeenCalledOnce())
    expect(confirmFalse).not.toHaveBeenCalled()

    const saveTrue = vi.fn(async () => configOK)
    const confirmTrue = vi.fn(async () => false)
    cleanup()
    await renderPage(dependencies({
      getHelperForm: async () => helperForm({ allowHardDelete: true }),
      saveHelper: saveTrue,
      confirmHardDelete: confirmTrue,
    }))
    expect(screen.getByLabelText('允许硬删除')).toBeChecked()
    expect(screen.getByRole('alert')).toHaveTextContent('高风险')
    await userEvent.setup().click(screen.getByRole('button', { name: '保存 Helper 配置' }))
    await waitFor(() => expect(confirmTrue).toHaveBeenCalledOnce())
    expect(saveTrue).not.toHaveBeenCalled()
    expect(screen.getByLabelText('允许硬删除')).toBeChecked()
  })

  it('manual 启动先提示管理员权限，等待期间禁用冲突动作，UAC 取消恢复并显示中性状态', async () => {
    let finish!: (value: typeof ok) => void
    const startHelper = vi.fn(() => new Promise<typeof ok>((resolve) => { finish = resolve }))
    await renderPage(dependencies({ startHelper }))
    expect(screen.getByText(/将请求管理员权限/)).toBeVisible()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '启动 Helper' }))
    expect(await screen.findByRole('status')).toHaveTextContent('等待系统确认')
    expect(screen.getByRole('button', { name: '停止 Helper' })).toBeDisabled()
    finish({ ok: false, errorCode: '', errorSummary: '', uacCancelled: true })
    await waitFor(() => expect(screen.getByText('已取消', { selector: '[role="status"]' })).toBeVisible())
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '启动 Helper' })).toBeEnabled()
  })

  it('automatic 只显示固定计划任务和漂移状态，不提供任意任务、账号或命令输入', async () => {
    await renderPage(dependencies({ getOverview: async () => overview({ helperStartMode: 'automatic', helperTaskDrift: true }) }))
    expect(screen.getByText('自动（固定计划任务）')).toBeVisible()
    expect(screen.getByText(/配置已漂移/)).toBeVisible()
    for (const name of ['计划任务名称', '运行账号', '命令', '命令参数']) {
      expect(screen.queryByLabelText(name)).not.toBeInTheDocument()
    }
  })

  it('Helper 禁用时保留可编辑保存表单，但禁用所有生命周期动作', async () => {
    await renderPage(dependencies({ getOverview: async () => overview({ helperEnabled: false }) }))
    expect(screen.getByText(/Helper 已禁用/)).toBeVisible()
    expect(screen.getByLabelText('管道名称')).toBeEnabled()
    expect(screen.getByRole('button', { name: '保存 Helper 配置' })).toBeEnabled()
    for (const name of ['启动 Helper', '停止 Helper', '重启 Helper']) {
      expect(screen.getByRole('button', { name })).toBeDisabled()
    }
  })

  it('程序设置保存发布共享快照后立即同步 Helper 策略与生命周期动作', async () => {
    let store = createTestNodeStore(async () => undefined)
    const start = vi.fn(async () => {
      if (start.mock.calls.length === 2) {
        store.publish({
          overview: overview({
            helperEnabled: true,
            helperStartMode: 'manual',
            helperTaskDrift: false,
            helper: component('stopped'),
          }),
        })
      }
    })
    store = createTestNodeStore(start)
    render(
      <NodeStateProvider store={store}>
        <SettingsPage
          dependencies={settingsDependencies()}
          onDirtyChange={() => undefined}
          onRequestExit={() => undefined}
        />
        <HelperPage dependencies={dependencies({
          getOverview: async () => overview({ helperEnabled: false, helperStartMode: 'manual' }),
        })} />
      </NodeStateProvider>,
    )
    const enabled = await screen.findByLabelText('启用 Helper')
    const startHelper = await screen.findByRole('button', { name: '启动 Helper' })
    await waitFor(() => expect(start).toHaveBeenCalledOnce())
    expect(screen.getByText('已禁用')).toBeVisible()
    expect(startHelper).toBeDisabled()

    const user = userEvent.setup()
    await user.click(enabled)
    await user.click(screen.getByRole('button', { name: '保存程序设置' }))

    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
    expect(screen.getByText('已启用')).toBeVisible()
    expect(screen.getByText('手动', { selector: 'dd' })).toBeVisible()
    expect(startHelper).toBeEnabled()
  })

  it('保存后需重启时使用后端摘要并显示明确提示', async () => {
    await renderPage(dependencies({ saveHelper: async () => ({ ...configOK, needsRestart: true }) }))
    await userEvent.setup().click(screen.getByRole('button', { name: '保存 Helper 配置' }))
    expect(await screen.findByText(/配置已保存，需要重启后生效/)).toHaveTextContent('dddddddddddd')
  })

  it('Helper 已落盘但应用失败时刷新共享状态后显示错误', async () => {
    const start = vi.fn(async () => undefined)
    const savedButFailed = { ...configOK, ok: false, saved: true, errorCode: 'fingerprint_update_failed' }
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <HelperPage dependencies={dependencies({ saveHelper: async () => savedButFailed })} />
      </NodeStateProvider>,
    )
    await screen.findByRole('heading', { name: '删除 Helper 配置' })
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(screen.getByRole('button', { name: '保存 Helper 配置' }))

    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
    expect(screen.getByRole('alert')).toBeVisible()
  })

  it('Helper 未落盘时不刷新共享状态', async () => {
    const start = vi.fn(async () => undefined)
    const failed = { ...configOK, ok: false, saved: false, errorCode: 'save_failed' }
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <HelperPage dependencies={dependencies({ saveHelper: async () => failed })} />
      </NodeStateProvider>,
    )
    await screen.findByRole('heading', { name: '删除 Helper 配置' })
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(screen.getByRole('button', { name: '保存 Helper 配置' }))

    await screen.findByRole('alert')
    expect(start).toHaveBeenCalledOnce()
  })

  it('Helper 保存等待共享刷新期间保持 pending，刷新结束后恢复', async () => {
    let finishRefresh!: () => void
    const refreshPending = new Promise<void>((resolve) => { finishRefresh = resolve })
    const start = vi.fn()
      .mockResolvedValueOnce(undefined)
      .mockImplementationOnce(() => refreshPending)
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <HelperPage dependencies={dependencies()} />
      </NodeStateProvider>,
    )
    const save = await screen.findByRole('button', { name: '保存 Helper 配置' })
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(save)

    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
    expect(save).toBeDisabled()
    finishRefresh()
    await waitFor(() => expect(save).toBeEnabled())
  })

  it('启动中只允许取消启动', async () => {
    await renderPage(dependencies({ getOverview: async () => overview({ helper: component('starting') }) }))
    expect(screen.getByRole('button', { name: '启动 Helper' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '取消启动' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '重启 Helper' })).toBeDisabled()
  })

  it('停止超时选择返回时不请求统一退出也不强制结束', async () => {
    const onRequestExit = vi.fn()
    const forceStopHelper = vi.fn(async () => ok)
    render(<HelperPage dependencies={dependencies({
      getOverview: async () => overview({ helper: component('running') }),
      stopHelper: async () => ({ ...ok, ok: false, errorCode: 'stop_timeout' }), forceStopHelper,
    })} onRequestExit={onRequestExit} />)
    await screen.findByRole('heading', { name: '删除 Helper 配置' })
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '停止 Helper' }))
    const dialog = await screen.findByRole('dialog', { name: 'Helper 停止超时' })
    await user.click(within(dialog).getByRole('button', { name: '返回' }))
    expect(onRequestExit).not.toHaveBeenCalled()
    expect(forceStopHelper).not.toHaveBeenCalled()
  })

  it('停止超时选择退出全部时只打开统一强制退出弹窗', async () => {
    const onRequestExit = vi.fn()
    const forceStopHelper = vi.fn(async () => ok)
    render(<HelperPage dependencies={dependencies({
      getOverview: async () => overview({ helper: component('running') }),
      stopHelper: async () => ({ ...ok, ok: false, errorCode: 'stop_timeout' }), forceStopHelper,
    })} onRequestExit={onRequestExit} />)
    await screen.findByRole('heading', { name: '删除 Helper 配置' })
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '停止 Helper' }))
    await user.click(within(await screen.findByRole('dialog', { name: 'Helper 停止超时' })).getByRole('button', { name: '强制退出全部' }))
    expect(onRequestExit).toHaveBeenCalledOnce()
    expect(forceStopHelper).not.toHaveBeenCalled()
  })

  it('停止超时的强制结束必须二次确认，取消不调用且永不自动调用', async () => {
    const forceStopHelper = vi.fn(async () => ok)
    const confirmForceStop = vi.fn(async () => false)
    await renderPage(dependencies({
      stopHelper: async () => ({ ...ok, ok: false, errorCode: 'stop_timeout' }),
      getOverview: async () => overview({ helper: component('running') }),
      forceStopHelper,
      confirmForceStop,
    }))
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '停止 Helper' }))
    expect(forceStopHelper).not.toHaveBeenCalled()
    await user.click(within(await screen.findByRole('dialog', { name: 'Helper 停止超时' })).getByRole('button', { name: '强制结束已认领 Helper' }))
    await waitFor(() => expect(confirmForceStop).toHaveBeenCalledOnce())
    expect(forceStopHelper).not.toHaveBeenCalled()

    confirmForceStop.mockResolvedValue(true)
    await user.click(screen.getByRole('button', { name: '强制结束已认领 Helper' }))
    await waitFor(() => expect(forceStopHelper).toHaveBeenCalledOnce())
  })
})
