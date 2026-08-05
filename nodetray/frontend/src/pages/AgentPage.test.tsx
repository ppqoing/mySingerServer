import '@testing-library/jest-dom/vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { config } from '../../wailsjs/go/models'
import type { WorkerState } from '../state/nodeStore'
import { NodeStateProvider } from '../state/NodeStateContext'
import { createTestNodeStore } from '../test/createTestNodeStore'
import { AgentPage, type AgentPageDependencies } from './AgentPage'

const ok = { ok: true, errorCode: '', errorSummary: '', uacCancelled: false }
const configOK = {
  ok: true, saved: true, restarted: false, sha256: 'a'.repeat(64), needsRestart: false,
  errorCode: '', errorSummary: '',
}

function formFixture(): config.AgentForm {
  return new config.AgentForm({
    listenHost: '127.0.0.1', listenPort: 18080,
    dataDir: 'C:\\fixtures\\agent-data',
    database: {
      host: 'db.internal', port: 5432, database: 'media', user: 'node_account',
      password: 'must-never-load', passwordStored: true, replacePassword: true, sslMode: 'require',
    },
    useEverything: false,
    scan: {
      hddReadBlockMb: 8, hddStreamsPerDisk: 2, ssdStreamsPerDisk: 4,
      imageMemResidentMb: 512, imageTimeoutS: 30, videoTimeoutS: 120,
      imageExts: ['.jpg'], videoExts: ['.mp4'],
    },
    sync: { intervalS: 10, triggerRows: 1000, upsertBatch: 500 },
    proto: { heartbeatS: 15 },
    worker: {
      count: 2, exePath: 'C:\\fixtures\\worker.exe', imageTimeoutS: 30,
      videoTimeoutS: 120, imageMemoryMb: 1024, respawnDelayMs: 1000,
    },
    pipeline: { readChunkKb: 256 },
    thumb: {
      cacheDir: 'C:\\fixtures\\thumbs', tileMaxSide: 640, probeTimeoutS: 10,
      nativeTimeoutS: 30, frameTimeoutS: 30,
    },
    ipc: { maxFrameMb: 32 },
    delete: {
      pipeName: 'fixture-delete-pipe', maxEntriesPerFrame: 100,
      dialTimeoutMs: 1000, helloTimeoutS: 5, reportTimeoutS: 30,
    },
    tuning: {
      statsEnabled: true, statsIntervalS: 10, statsHistoryS: 300,
      pendingBytesMb: 128, statsLogMb: 16, pprofAddr: '127.0.0.1:0',
    },
  })
}

function dependencies(overrides: Partial<AgentPageDependencies> = {}): AgentPageDependencies {
  return {
    getAgentForm: vi.fn(async () => formFixture()),
    validateAgent: vi.fn(async (agentForm: config.AgentForm) => { void agentForm; return [] }),
    saveAgent: vi.fn(async (agentForm: config.AgentForm) => { void agentForm; return configOK }),
    saveAndRestartAgent: vi.fn(async (agentForm: config.AgentForm) => { void agentForm; return { ...configOK, restarted: true } }),
    startAgent: vi.fn(async () => ok),
    stopAgent: vi.fn(async () => ok),
    restartAgent: vi.fn(async () => ok),
    confirmRestart: vi.fn(async () => true),
    copyText: vi.fn(async (text: string) => { void text }),
    choosePath: vi.fn(async (currentPath: string) => { void currentPath; return '' }),
    ...overrides,
  }
}

async function renderPage(deps = dependencies(), workers: WorkerState[] = [], lifecycle = 'running') {
  render(<AgentPage dependencies={deps} workers={workers} workerReady={0} workerExpected={workers.length} componentState={{ lifecycle }} />)
  await screen.findByDisplayValue('127.0.0.1')
  return deps
}

describe('AgentPage', () => {
  it('只在表单 dirty 状态变化时向壳上报', async () => {
    const onDirtyChange = vi.fn()
    render(<AgentPage dependencies={dependencies()} onDirtyChange={onDirtyChange} />)
    const listenHost = await screen.findByLabelText('监听地址')
    await waitFor(() => expect(onDirtyChange).toHaveBeenLastCalledWith(false))
    await userEvent.setup().type(listenHost, '-changed')
    await waitFor(() => expect(onDirtyChange).toHaveBeenLastCalledWith(true))
  })

  it('用结构化控件覆盖 AgentForm 全字段且不暴露 crash injection 或 JSON 编辑器', async () => {
    await renderPage()
    const names = [
      'listenHost', 'listenPort', 'dataDir',
      'database.host', 'database.port', 'database.database', 'database.user',
      'database.password', 'database.sslMode', 'useEverything',
      'scan.hddReadBlockMb', 'scan.hddStreamsPerDisk', 'scan.ssdStreamsPerDisk',
      'scan.imageMemResidentMb', 'scan.imageTimeoutS', 'scan.videoTimeoutS',
      'scan.imageExts', 'scan.videoExts', 'sync.intervalS', 'sync.triggerRows',
      'sync.upsertBatch', 'proto.heartbeatS', 'worker.count', 'worker.exePath',
      'worker.imageTimeoutS', 'worker.videoTimeoutS', 'worker.imageMemoryMb',
      'worker.respawnDelayMs', 'pipeline.readChunkKb', 'thumb.cacheDir',
      'thumb.tileMaxSide', 'thumb.probeTimeoutS', 'thumb.nativeTimeoutS',
      'thumb.frameTimeoutS', 'ipc.maxFrameMb', 'delete.pipeName',
      'delete.maxEntriesPerFrame', 'delete.dialTimeoutMs', 'delete.helloTimeoutS',
      'delete.reportTimeoutS', 'tuning.statsEnabled', 'tuning.statsIntervalS',
      'tuning.statsHistoryS', 'tuning.pendingBytesMb', 'tuning.statsLogMb',
      'tuning.pprofAddr',
    ]
    for (const name of names) {
      expect(document.querySelector(`[name="${name}"]`), `missing ${name}`).toBeInTheDocument()
    }
    expect(screen.queryByLabelText('机器 ID')).not.toBeInTheDocument()

    const password = document.querySelector<HTMLInputElement>('input[name="database.password"]')
    expect(password).toHaveAttribute('type', 'password')
    expect(password).toBeDisabled()
    expect(password).toHaveValue('')
    expect(document.body).not.toHaveTextContent(/crash[_ -]?injection/i)
    expect(screen.queryByRole('textbox', { name: /JSON/i })).not.toBeInTheDocument()
  })

  it('密码只有显式替换后可编辑，取消立即清空且诊断不包含秘密、数据库身份或路径', async () => {
    const copyText = vi.fn(async (text: string) => { void text })
    await renderPage(dependencies({ copyText }))
    const user = userEvent.setup()

    expect(screen.getByText('已保存，留空保留')).toBeVisible()
    await user.click(screen.getByRole('button', { name: '替换密码' }))
    const password = screen.getByLabelText('数据库密码')
    expect(password).toBeEnabled()
    await user.type(password, 'new-password-value')
    expect(password).toHaveValue('new-password-value')
    await user.click(screen.getByRole('button', { name: '取消替换密码' }))
    expect(password).toBeDisabled()
    expect(password).toHaveValue('')

    await user.click(screen.getByRole('button', { name: '复制配置诊断' }))
    await waitFor(() => expect(copyText).toHaveBeenCalledOnce())
    const diagnostic = String(copyText.mock.calls[0][0])
    for (const forbidden of ['must-never-load', 'new-password-value', 'node_account', 'db.internal', 'C:\\fixtures']) {
      expect(diagnostic).not.toContain(forbidden)
    }
    expect(diagnostic).toContain('passwordStored=true')
    expect(diagnostic).toContain('replacePassword=false')
  })

  it('失焦执行类型化校验并把 FieldError 定位到对应字段', async () => {
    const validateAgent = vi.fn(async (agentForm: config.AgentForm) => {
      void agentForm
      return [
        { field: 'database.port', code: 'range', message: '端口超出范围' },
        { field: 'worker.count', code: 'range', message: '数量超出范围' },
      ]
    })
    await renderPage(dependencies({ validateAgent }))

    fireEvent.blur(screen.getByLabelText('数据库端口'))
    await waitFor(() => expect(validateAgent).toHaveBeenCalledOnce())
    expect(screen.getByLabelText('数据库端口')).toHaveAccessibleErrorMessage('端口超出范围')
    expect(screen.queryByText('数量超出范围')).not.toBeInTheDocument()
  })

  it('保存先完整校验，只调用 SaveAgent；失败保持 dirty 和密码输入且错误不回显后端敏感文本', async () => {
    const validateAgent = vi.fn(async (agentForm: config.AgentForm) => { void agentForm; return [] })
    const saveAgent = vi.fn(async (agentForm: config.AgentForm) => {
      void agentForm
      return {
        ...configOK, ok: false, saved: false, errorCode: 'save_failed',
        errorSummary: 'new-password-value C:\\fixtures\\agent-data',
      }
    })
    const deps = dependencies({ validateAgent, saveAgent })
    await renderPage(deps, [], 'stopped')
    const user = userEvent.setup()
    await user.clear(screen.getByLabelText('监听地址'))
    await user.type(screen.getByLabelText('监听地址'), '127.0.0.2')
    await user.click(screen.getByRole('button', { name: '替换密码' }))
    await user.type(screen.getByLabelText('数据库密码'), 'new-password-value')

    const validationsBeforeSave = validateAgent.mock.calls.length
    await user.click(screen.getByRole('button', { name: '保存配置' }))
    await waitFor(() => expect(saveAgent).toHaveBeenCalledOnce())
    expect(validateAgent.mock.calls.length).toBeGreaterThan(validationsBeforeSave)
    expect(validateAgent.mock.calls.at(-1)?.[0]).toMatchObject({
      listenHost: '127.0.0.2',
      database: { replacePassword: true },
    })
    expect(validateAgent.mock.calls.at(-1)?.[0]).not.toHaveProperty('machineId')
    expect(deps.saveAndRestartAgent).not.toHaveBeenCalled()
    expect(deps.restartAgent).not.toHaveBeenCalled()
    expect(screen.getByLabelText('数据库密码')).toHaveValue('new-password-value')
    expect(screen.getByText('存在未保存更改')).toBeVisible()
    expect(document.body).not.toHaveTextContent('C:\\fixtures\\agent-data')
    expect(screen.getByRole('alert')).not.toHaveTextContent('new-password-value')
  })

  it('Agent 已落盘但后续失败时仍刷新并显示错误', async () => {
    const start = vi.fn(async () => undefined)
    const deps = dependencies({
      saveAgent: vi.fn(async () => ({
        ...configOK,
        ok: false,
        saved: true,
        errorCode: 'fingerprint_update_failed',
      })),
    })
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <AgentPage dependencies={deps} componentState={{ lifecycle: 'stopped' }} />
      </NodeStateProvider>,
    )
    await screen.findByDisplayValue('127.0.0.1')
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(screen.getByRole('button', { name: '保存配置' }))

    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
    expect(screen.getByRole('alert')).toBeVisible()
    expect(screen.getByText('配置未修改')).toBeVisible()
  })

  it('Agent 未落盘时不刷新共享状态', async () => {
    const start = vi.fn(async () => undefined)
    const deps = dependencies({
      saveAgent: vi.fn(async () => ({
        ...configOK,
        ok: false,
        saved: false,
        errorCode: 'save_failed',
      })),
    })
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <AgentPage dependencies={deps} componentState={{ lifecycle: 'stopped' }} />
      </NodeStateProvider>,
    )
    await screen.findByDisplayValue('127.0.0.1')
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(screen.getByRole('button', { name: '保存配置' }))

    await screen.findByRole('alert')
    expect(start).toHaveBeenCalledOnce()
  })

  it('Agent 保存等待共享刷新期间保持 pending，刷新结束后恢复', async () => {
    let finishRefresh!: () => void
    const refreshPending = new Promise<void>((resolve) => { finishRefresh = resolve })
    const start = vi.fn()
      .mockResolvedValueOnce(undefined)
      .mockImplementationOnce(() => refreshPending)
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <AgentPage dependencies={dependencies()} componentState={{ lifecycle: 'stopped' }} />
      </NodeStateProvider>,
    )
    const save = await screen.findByRole('button', { name: '保存配置' })
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(save)

    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
    expect(save).toBeDisabled()
    finishRefresh()
    await waitFor(() => expect(save).toBeEnabled())
  })

  it('保存并重启已落盘但停止失败时刷新共享状态并显示错误', async () => {
    const start = vi.fn(async () => undefined)
    const saveAndRestartAgent = vi.fn(async () => ({
      ...configOK,
      ok: false,
      saved: true,
      restarted: false,
      needsRestart: true,
      errorCode: 'stop_timeout',
    }))
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <AgentPage dependencies={dependencies({ saveAndRestartAgent })} componentState={{ lifecycle: 'running' }} />
      </NodeStateProvider>,
    )
    await screen.findByDisplayValue('127.0.0.1')
    await waitFor(() => expect(start).toHaveBeenCalledOnce())

    await userEvent.setup().click(screen.getByRole('button', { name: '保存并重启 Agent' }))

    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
    expect(saveAndRestartAgent).toHaveBeenCalledOnce()
    expect(screen.getByRole('alert')).toHaveTextContent('stop_timeout')
    expect(screen.getByRole('status')).toHaveTextContent('配置已保存，需要重启后生效')
  })

  it('保存并重启仅在 dirty 时确认并单次调用 SaveAndRestartAgent，取消不调用', async () => {
    const confirmRestart = vi.fn(async () => false)
    const start = vi.fn(async () => undefined)
    const saveAndRestartAgent = vi.fn(async (agentForm: config.AgentForm) => { void agentForm; return { ...configOK, restarted: true } })
    const deps = dependencies({ confirmRestart, saveAndRestartAgent })
    render(
      <NodeStateProvider store={createTestNodeStore(start)}>
        <AgentPage dependencies={deps} componentState={{ lifecycle: 'running' }} />
      </NodeStateProvider>,
    )
    await screen.findByDisplayValue('127.0.0.1')
    await waitFor(() => expect(start).toHaveBeenCalledOnce())
    const user = userEvent.setup()
    await user.clear(screen.getByLabelText('监听端口'))
    await user.type(screen.getByLabelText('监听端口'), '18081')

    await user.click(screen.getByRole('button', { name: '保存并重启 Agent' }))
    await waitFor(() => expect(confirmRestart).toHaveBeenCalledOnce())
    expect(saveAndRestartAgent).not.toHaveBeenCalled()

    confirmRestart.mockResolvedValue(true)
    await user.click(screen.getByRole('button', { name: '保存并重启 Agent' }))
    await waitFor(() => expect(saveAndRestartAgent).toHaveBeenCalledOnce())
    expect(deps.saveAgent).not.toHaveBeenCalled()
    expect(deps.restartAgent).not.toHaveBeenCalled()
    await waitFor(() => expect(start).toHaveBeenCalledTimes(2))
  })

  it('成功保存会 commit、清理密码编辑态并仅显示后端返回的 SHA-256 短摘要', async () => {
    await renderPage(dependencies({ saveAgent: vi.fn(async () => ({ ...configOK, sha256: 'c'.repeat(64) })) }))
    const user = userEvent.setup()
    await user.clear(screen.getByLabelText('监听地址'))
    await user.type(screen.getByLabelText('监听地址'), '127.0.0.3')
    await user.click(screen.getByRole('button', { name: '替换密码' }))
    await user.type(screen.getByLabelText('数据库密码'), 'temporary-value')
    await user.click(screen.getByRole('button', { name: '保存配置' }))

    expect(await screen.findByText('配置已保存（cccccccccccc）。')).toBeVisible()
    expect(screen.getByText('配置未修改')).toBeVisible()
    expect(screen.getByLabelText('数据库密码')).toBeDisabled()
    expect(screen.getByLabelText('数据库密码')).toHaveValue('')
  })

  it('配置已保存但未重启时明确提示需重启，不声称重启成功', async () => {
    const saveAgent = vi.fn(async () => ({ ...configOK, needsRestart: true }))
    await renderPage(dependencies({ saveAgent }))
    await userEvent.setup().click(screen.getByRole('button', { name: '保存配置' }))
    expect(await screen.findByRole('status')).toHaveTextContent('配置已保存，需要重启后生效')
    expect(screen.getByRole('status')).not.toHaveTextContent('重启成功')
  })

  it('启动中只允许取消启动，禁用启动、重启和保存并重启', async () => {
    await renderPage(dependencies(), [], 'starting')
    expect(screen.getByRole('button', { name: '启动 Agent' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '取消启动' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '重启 Agent' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '保存并重启 Agent' })).toBeDisabled()
  })

  it('路径选择只接受注入回调返回的非空本地绝对路径', async () => {
    const choosePath = vi.fn()
      .mockResolvedValueOnce('relative-folder')
      .mockResolvedValueOnce('')
      .mockResolvedValueOnce('C:\\fixtures\\picked')
    await renderPage(dependencies({ choosePath }))
    const user = userEvent.setup()

    const input = screen.getByLabelText('数据目录')
    await user.click(screen.getByRole('button', { name: '选择数据目录' }))
    expect(input).toHaveValue('C:\\fixtures\\agent-data')
    await user.click(screen.getByRole('button', { name: '选择数据目录' }))
    expect(input).toHaveValue('C:\\fixtures\\agent-data')
    await user.click(screen.getByRole('button', { name: '选择数据目录' }))
    expect(input).toHaveValue('C:\\fixtures\\picked')
  })

  it('扩展名标签按键规范化、去重并可删除', async () => {
    await renderPage()
    const user = userEvent.setup()
    await user.click(screen.getByText('高级设置'))
    const input = screen.getByLabelText('图片扩展名')
    await user.type(input, 'PNG{Enter}')
    expect(screen.getByText('.png')).toBeVisible()
    await user.type(input, '.PNG{Enter}')
    expect(screen.getAllByText('.png')).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: '删除 .png' }))
    expect(screen.queryByText('.png')).not.toBeInTheDocument()
  })

  it('顶部仅提供 Agent 生命周期动作，Worker 摘要严格只读', async () => {
    const deps = dependencies()
    await renderPage(deps, [{ index: 0, pid: 2101, ready: true, currentTaskSummary: '等待任务', lastErrorSummary: '' }])
    expect(screen.getByRole('button', { name: '启动 Agent' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '停止 Agent' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '重启 Agent' })).toBeEnabled()

    const worker = screen.getByRole('region', { name: 'Worker 状态' })
    expect(within(worker).queryByRole('button')).not.toBeInTheDocument()
    for (const label of ['启动 Worker', '停止 Worker', '重启 Worker', '删除 Worker']) {
      expect(within(worker).queryByText(label)).not.toBeInTheDocument()
    }
  })

  it('重复启动 Agent 显示已运行状态而不是操作失败', async () => {
    const deps = dependencies({
      startAgent: vi.fn(async () => ({
        ok: false,
        errorCode: 'already_running',
        errorSummary: 'component is already active',
        uacCancelled: false,
      })),
    })
    await renderPage(deps, [], 'stopped')

    await userEvent.setup().click(screen.getByRole('button', { name: '启动 Agent' }))

    expect(await screen.findByRole('status')).toHaveTextContent('Agent 已在运行。')
    expect(screen.queryByText('操作失败（already_running）。')).not.toBeInTheDocument()
  })
})
