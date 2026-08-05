import '@testing-library/jest-dom/vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createNodeStore, type NodeOverview, type NodeStoreDependencies } from '../state/nodeStore'
import { OverviewPage, type OverviewActions } from './OverviewPage'

function component(lifecycle: string, pid: number) {
  return {
    lifecycle,
    healthy: lifecycle === 'running',
    ready: lifecycle === 'running',
    pid,
    startedAtUnixMs: lifecycle === 'running' ? 1000 : 0,
    uptimeSeconds: lifecycle === 'running' ? 3661 : 0,
    workerReady: 1,
    workerExpected: 2,
    activeRequests: 0,
    errorCode: '',
    errorSummary: '',
    needsAttention: false,
    runtimeConfigSha256: lifecycle === 'running' ? 'b'.repeat(64) : '',
    savedConfigSha256: 'a'.repeat(64),
    needsRestart: lifecycle === 'running',
  }
}

function overview(): NodeOverview {
  return {
    machineId: 'node-' + 'a'.repeat(64),
    agent: component('running', 1101),
    workers: [
      { index: 0, pid: 2101, ready: true, currentTaskSummary: '等待任务', lastErrorSummary: '' },
      { index: 1, pid: 2102, ready: false, currentTaskSummary: '处理队列', lastErrorSummary: '任务超时' },
    ],
    helper: component('stopped', 0),
    agentStartMode: 'automatic',
    helperStartMode: 'manual',
    helperEnabled: true,
    helperTaskDrift: true,
    loginStartDrift: false,
  }
}

function testStore(value = overview()) {
  const dependencies: NodeStoreDependencies = {
    getOverview: async () => value,
    onEvent: () => () => undefined,
  }
  return createNodeStore(dependencies)
}

function successfulActions(): OverviewActions {
  const success = async () => ({ ok: true, errorCode: '', errorSummary: '', uacCancelled: false })
  return {
    startAgent: success,
    stopAgent: success,
    restartAgent: success,
    startHelper: success,
    stopHelper: success,
    restartHelper: success,
  }
}

describe('OverviewPage', () => {
  it('显示 Agent、Worker 和删除 Helper 的完整只读摘要与 UAC 提示', async () => {
    render(<OverviewPage store={testStore()} actions={successfulActions()} />)

    const agent = await screen.findByRole('article', { name: 'Agent' })
    expect(within(agent).getByText('node-' + 'a'.repeat(64))).toBeVisible()
    expect(within(agent).getByText('PID')).toBeVisible()
    expect(within(agent).getByText('1101')).toBeVisible()
    expect(agent).toHaveTextContent('自动')
    expect(agent).toHaveTextContent('1 小时 1 分 1 秒')
    expect(within(agent).getByText('运行配置').parentElement).toHaveTextContent('bbbbbbbbbbbb')
    expect(within(agent).getByText('已保存配置').parentElement).toHaveTextContent('aaaaaaaaaaaa')
    expect(agent).toHaveTextContent('需要重启后生效')
    expect(screen.getByText('1 / 2')).toBeVisible()
    expect(screen.getByText('任务超时')).toBeVisible()

    const helper = screen.getByRole('article', { name: '删除 Helper' })
    expect(helper).toHaveTextContent('已启用')
    expect(helper).toHaveTextContent('手动')
    expect(screen.getByText(/计划任务配置已漂移.*UAC/)).toBeVisible()
  })

  it('启动中只允许取消启动并禁用其他冲突动作', async () => {
    const value = overview()
    value.agent = component('starting', 1101)
    render(<OverviewPage store={testStore(value)} actions={successfulActions()} />)
    const agent = await screen.findByRole('article', { name: 'Agent' })
    expect(within(agent).getByRole('button', { name: '启动 Agent' })).toBeDisabled()
    expect(within(agent).getByRole('button', { name: '取消启动' })).toBeEnabled()
    expect(within(agent).getByRole('button', { name: '重启 Agent' })).toBeDisabled()
  })

  it('组件动作 pending 时只禁用该组件动作，且不乐观改写生命周期', async () => {
    const user = userEvent.setup()
    let finish!: (value: { ok: boolean; errorCode: string; errorSummary: string; uacCancelled: boolean }) => void
    const pending = new Promise<{ ok: boolean; errorCode: string; errorSummary: string; uacCancelled: boolean }>(
      (resolve) => {
        finish = resolve
      },
    )
    const actions = successfulActions()
    actions.stopAgent = () => pending
    render(<OverviewPage store={testStore()} actions={actions} />)

    const agent = await screen.findByRole('article', { name: 'Agent' })
    const helper = screen.getByRole('article', { name: '删除 Helper' })
    await user.click(within(agent).getByRole('button', { name: '停止 Agent' }))

    for (const button of within(agent).getAllByRole('button')) {
      expect(button).toBeDisabled()
    }
    expect(within(helper).getByRole('button', { name: '启动 Helper' })).toBeEnabled()
    expect(agent).toHaveTextContent('运行中')

    finish({ ok: true, errorCode: '', errorSummary: '', uacCancelled: false })
    await waitFor(() => expect(within(agent).getByRole('button', { name: '停止 Agent' })).toBeEnabled())
    expect(agent).toHaveTextContent('运行中')
  })

  it('Agent 与 Helper 并发操作各自保持 pending，任一完成不解除另一组件', async () => {
    const user = userEvent.setup()
    type Result = { ok: boolean; errorCode: string; errorSummary: string; uacCancelled: boolean }
    let finishAgent!: (value: Result) => void
    let finishHelper!: (value: Result) => void
    const agentPending = new Promise<Result>((resolve) => {
      finishAgent = resolve
    })
    const helperPending = new Promise<Result>((resolve) => {
      finishHelper = resolve
    })
    const actions = successfulActions()
    actions.stopAgent = () => agentPending
    actions.startHelper = () => helperPending
    render(<OverviewPage store={testStore()} actions={actions} />)

    const agent = await screen.findByRole('article', { name: 'Agent' })
    const helper = screen.getByRole('article', { name: '删除 Helper' })
    await user.click(within(agent).getByRole('button', { name: '停止 Agent' }))
    await user.click(within(helper).getByRole('button', { name: '启动 Helper' }))

    expect(within(agent).getByRole('button', { name: '停止 Agent' })).toBeDisabled()
    expect(within(helper).getByRole('button', { name: '启动 Helper' })).toBeDisabled()

    finishHelper({ ok: true, errorCode: '', errorSummary: '', uacCancelled: false })
    await waitFor(() => expect(within(helper).getByRole('button', { name: '启动 Helper' })).toBeEnabled())
    expect(within(agent).getByRole('button', { name: '停止 Agent' })).toBeDisabled()

    finishAgent({ ok: true, errorCode: '', errorSummary: '', uacCancelled: false })
    await waitFor(() => expect(within(agent).getByRole('button', { name: '停止 Agent' })).toBeEnabled())
  })

  it('OperationResult 失败只在页内显示脱敏摘要', async () => {
    const user = userEvent.setup()
    const actions = successfulActions()
    actions.startHelper = async () => ({
      ok: false,
      errorCode: 'helper_failed',
      errorSummary: 'token=visible\r\n启动失败',
      uacCancelled: false,
    })
    render(<OverviewPage store={testStore()} actions={actions} />)

    const helper = await screen.findByRole('article', { name: '删除 Helper' })
    await user.click(within(helper).getByRole('button', { name: '启动 Helper' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('[REDACTED] 启动失败')
    expect(alert).not.toHaveTextContent('visible')
    expect(helper).toHaveTextContent('已停止')
  })

  it('重复启动 Agent 显示已运行状态而不是操作失败', async () => {
    const user = userEvent.setup()
    const actions = successfulActions()
    actions.startAgent = async () => ({
      ok: false,
      errorCode: 'already_running',
      errorSummary: 'component is already active',
      uacCancelled: false,
    })
    const value = overview()
    value.agent = component('stopped', 0)
    render(<OverviewPage store={testStore(value)} actions={actions} />)

    const agent = await screen.findByRole('article', { name: 'Agent' })
    await user.click(within(agent).getByRole('button', { name: '启动 Agent' }))

    expect(await screen.findByRole('status')).toHaveTextContent('Agent 已在运行。')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('Helper 禁用且无 PID 时显示未启用、清空最近异常并禁用全部操作', async () => {
    const value = overview()
    value.helperEnabled = false
    value.helper = {
      ...component('failed', 0),
      errorCode: 'unavailable', errorSummary: '组件不可用', needsAttention: true,
    }
    value.helperTaskDrift = true
    render(<OverviewPage store={testStore(value)} actions={successfulActions()} />)

    const helper = await screen.findByRole('article', { name: '删除 Helper' })
    expect(helper).toHaveTextContent('未启用')
    expect(within(helper).queryByText('异常')).not.toBeInTheDocument()
    expect(within(helper).getByText('最近异常').parentElement).toHaveTextContent('—')
    expect(helper).not.toHaveTextContent('组件不可用')
    expect(helper).toHaveTextContent('计划任务配置已漂移')
    for (const button of within(helper).getAllByRole('button')) {
      expect(button).toBeDisabled()
    }
  })

  it('Helper 禁用但有真实 PID 时显示运行态并允许停止', async () => {
    const value = overview()
    value.helperEnabled = false
    value.helper = component('running', 3301)
    render(<OverviewPage store={testStore(value)} actions={successfulActions()} />)

    const helper = await screen.findByRole('article', { name: '删除 Helper' })
    expect(helper).toHaveTextContent('运行中')
    expect(helper).toHaveTextContent('3301')
    expect(within(helper).getByRole('button', { name: '启动 Helper' })).toBeDisabled()
    expect(within(helper).getByRole('button', { name: '停止 Helper' })).toBeEnabled()
    expect(within(helper).getByRole('button', { name: '重启 Helper' })).toBeDisabled()
  })
})
