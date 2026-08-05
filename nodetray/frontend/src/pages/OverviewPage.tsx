import { useState, type ReactNode } from 'react'
import {
  RestartAgent,
  RestartHelper,
  StartAgent,
  StartHelper,
  StopAgent,
  StopHelper,
} from '../../wailsjs/go/main/Backend'
import { ActionBar } from '../components/ActionBar'
import { ComponentCard } from '../components/ComponentCard'
import { StatusBadge } from '../components/StatusBadge'
import { WorkerSummary } from '../components/WorkerSummary'
import { NodeStateProvider, useNodeState } from '../state/NodeStateContext'
import { sanitizeSummary, type ComponentName, type NodeStore } from '../state/nodeStore'

type OperationResult = {
  ok: boolean
  errorCode: string
  errorSummary: string
  uacCancelled: boolean
}

export type OverviewActions = {
  startAgent: () => Promise<OperationResult>
  stopAgent: () => Promise<OperationResult>
  restartAgent: () => Promise<OperationResult>
  startHelper: () => Promise<OperationResult>
  stopHelper: () => Promise<OperationResult>
  restartHelper: () => Promise<OperationResult>
}

type OverviewPageProps = {
  store?: NodeStore
  actions?: OverviewActions
}

const productionActions: OverviewActions = {
  startAgent: StartAgent,
  stopAgent: StopAgent,
  restartAgent: RestartAgent,
  startHelper: StartHelper,
  stopHelper: StopHelper,
  restartHelper: RestartHelper,
}

export function OverviewPage({ store, actions = productionActions }: OverviewPageProps): ReactNode {
  if (store) {
    return <NodeStateProvider store={store}><OverviewContent actions={actions} /></NodeStateProvider>
  }
  return <OverviewContent actions={actions} />
}

function OverviewContent({ actions }: { actions: OverviewActions }): ReactNode {
  const { snapshot, refresh } = useNodeState()
  const [pending, setPending] = useState<ReadonlySet<ComponentName>>(() => new Set())
  const [actionError, setActionError] = useState('')
  const [actionStatus, setActionStatus] = useState('')

  const runAction = async (component: ComponentName, action: () => Promise<OperationResult>) => {
    setPending((current) => new Set(current).add(component))
    setActionError('')
    setActionStatus('')
    try {
      const result = await action()
      if (!result.ok) {
        if (result.errorCode === 'operation_conflict') void refresh()
        if (result.errorCode === 'already_running') {
          setActionStatus(`${component === 'agent' ? 'Agent' : 'Helper'} 已在运行。`)
        } else {
          setActionError(sanitizeSummary(result.errorSummary) || '操作失败，请稍后重试。')
        }
      }
    } catch {
      setActionError('操作失败，请稍后重试。')
    } finally {
      setPending((current) => {
        const next = new Set(current)
        next.delete(component)
        return next
      })
    }
  }

  const overview = snapshot.overview
  if (!overview) {
    return (
      <section aria-label="节点总览">
        <h2>节点总览</h2>
        {snapshot.loading ? <p role="status">正在读取节点状态…</p> : null}
        {snapshot.errorSummary ? <p role="alert">{snapshot.errorSummary}</p> : null}
      </section>
    )
  }

  const visibleError = actionError || snapshot.errorSummary || snapshot.attention?.summary || ''
  const agentActions = lifecycleActions(overview.agent.lifecycle)
  const helperActions = lifecycleActions(overview.helper.lifecycle)
  const helperDisabled = !overview.helperEnabled && overview.helper.pid <= 0

  return (
    <section aria-label="节点总览">
      <h2>节点总览</h2>
      {visibleError ? <p role="alert">{visibleError}</p> : null}
      {actionStatus ? <p role="status">{actionStatus}</p> : null}
      {snapshot.operation?.summary ? <p role="status">{snapshot.operation.summary}</p> : null}

      <ComponentCard
        title="Agent"
        status={<StatusBadge lifecycle={overview.agent.lifecycle} />}
        pending={pending.has('agent')}
        summary={
          <dl>
            <div><dt>机器 ID</dt><dd className="mono" title={overview.machineId}>{overview.machineId || '—'}</dd></div>
            <div><dt>PID</dt><dd>{overview.agent.pid > 0 ? overview.agent.pid : '—'}</dd></div>
            <div><dt>启动方式</dt><dd>{startModeLabel(overview.agentStartMode)}</dd></div>
            <div><dt>运行时长</dt><dd>{formatUptime(overview.agent.uptimeSeconds)}</dd></div>
            <div><dt>运行配置</dt><dd title={overview.agent.runtimeConfigSha256}>{shortDigest(overview.agent.runtimeConfigSha256)}</dd></div>
            <div><dt>已保存配置</dt><dd title={overview.agent.savedConfigSha256}>{shortDigest(overview.agent.savedConfigSha256)}</dd></div>
            <div><dt>最近异常</dt><dd>{overview.agent.errorSummary || '—'}</dd></div>
            {overview.agent.needsRestart ? <div><dt>配置状态</dt><dd>需要重启后生效</dd></div> : null}
          </dl>
        }
        actions={
          <ActionBar ariaLabel="Agent 操作">
            <button className="button-primary" type="button" disabled={!agentActions.start} onClick={() => void runAction('agent', actions.startAgent)}>启动 Agent</button>
            <button className="button-secondary" type="button" disabled={!agentActions.stop} onClick={() => void runAction('agent', actions.stopAgent)}>{agentActions.cancelStart ? '取消启动' : '停止 Agent'}</button>
            <button className="button-secondary" type="button" disabled={!agentActions.restart} onClick={() => void runAction('agent', actions.restartAgent)}>重启 Agent</button>
          </ActionBar>
        }
      />

      <WorkerSummary
        ready={overview.agent.workerReady}
        expected={overview.agent.workerExpected}
        workers={overview.workers}
      />

      <ComponentCard
        title="删除 Helper"
        status={<StatusBadge lifecycle={overview.helper.lifecycle} disabled={helperDisabled} />}
        pending={pending.has('helper')}
        summary={
          <div>
            <dl>
              <div><dt>启用状态</dt><dd>{overview.helperEnabled ? '已启用' : '未启用'}</dd></div>
              <div><dt>启动方式</dt><dd>{startModeLabel(overview.helperStartMode)}</dd></div>
              <div><dt>PID</dt><dd>{overview.helper.pid > 0 ? overview.helper.pid : '—'}</dd></div>
              <div><dt>运行配置</dt><dd title={overview.helper.runtimeConfigSha256}>{shortDigest(overview.helper.runtimeConfigSha256)}</dd></div>
              <div><dt>已保存配置</dt><dd title={overview.helper.savedConfigSha256}>{shortDigest(overview.helper.savedConfigSha256)}</dd></div>
              <div><dt>最近异常</dt><dd>{helperDisabled ? '—' : overview.helper.errorSummary || '—'}</dd></div>
              {overview.helper.needsRestart ? <div><dt>配置状态</dt><dd>需要重启后生效</dd></div> : null}
            </dl>
            {overview.helperTaskDrift ? <p>计划任务配置已漂移；下一次 Helper 操作可能需要 UAC。</p> : null}
          </div>
        }
        actions={
          <ActionBar ariaLabel="删除 Helper 操作">
            <button className="button-primary" type="button" disabled={!overview.helperEnabled || !helperActions.start} onClick={() => void runAction('helper', actions.startHelper)}>启动 Helper</button>
            <button className="button-secondary" type="button" disabled={helperDisabled || !helperActions.stop} onClick={() => void runAction('helper', actions.stopHelper)}>{helperActions.cancelStart ? '取消启动' : '停止 Helper'}</button>
            <button className="button-secondary" type="button" disabled={!overview.helperEnabled || !helperActions.restart} onClick={() => void runAction('helper', actions.restartHelper)}>重启 Helper</button>
          </ActionBar>
        }
      />
    </section>
  )
}

export function lifecycleActions(lifecycle: string) {
  return {
    start: lifecycle === 'stopped' || lifecycle === 'failed',
    stop: lifecycle === 'running' || lifecycle === 'starting',
    restart: lifecycle === 'running',
    cancelStart: lifecycle === 'starting',
  }
}

function shortDigest(value: string): string {
  return value ? value.slice(0, 12) : '—'
}

function startModeLabel(value: string): string {
  if (value === 'automatic') {
    return '自动'
  }
  if (value === 'manual') {
    return '手动'
  }
  return '未知'
}

function formatUptime(value: number): string {
  const seconds = Math.max(0, Math.floor(value))
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const rest = seconds % 60
  return `${hours} 小时 ${minutes} 分 ${rest} 秒`
}
