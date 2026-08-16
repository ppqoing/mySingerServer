import type { ReactNode } from 'react'
import {
  actionsForTaskStatus,
  phaseLabelForTask,
  statusLabelForTask,
  type LocalTask,
  type LocalTaskControl,
  type LocalTaskOperation,
} from './localTaskLifecycle'

const actionLabel = (operation: LocalTaskOperation, status: string): string => {
  if (operation === 'pause') return '暂停'
  if (operation === 'resume') return '继续'
  if (operation === 'cancel') return '停止'
  if (operation === 'retry') return '重试'
  return status === 'delete_failed' ? '重试删除' : '删除'
}

const modeLabel = (mode: string): string => {
  if (mode === 'scan_only') return '仅扫描'
  if (mode === 'scan_then_analysis') return '扫描并自动一、二、三筛'
  return '未知模式'
}

const createdAtLabel = (createdAt: number): string => Number.isFinite(createdAt) && createdAt > 0
  ? new Date(createdAt).toLocaleString('zh-CN', { hour12: false })
  : '创建时间未知'

const safeProgress = (complete: number, total: number): { value: number; max: number } => {
  const max = Number.isFinite(total) && total > 0 ? total : 1
  const value = Number.isFinite(complete) ? Math.min(Math.max(complete, 0), max) : 0
  return { value, max }
}

export function LocalTaskItem({
  task,
  locked,
  onAction,
}: {
  task: LocalTask
  locked: boolean
  onAction: (operation: LocalTaskOperation, control: LocalTaskControl) => void
}): ReactNode {
  const actions = actionsForTaskStatus(task.status)
  const control: LocalTaskControl = {
    taskId: task.taskId,
    instanceId: task.instanceId,
    expectedRevision: task.revision,
  }
  const complete = Number.isFinite(task.progressComplete) ? Math.max(0, task.progressComplete) : 0
  const knownProgress = task.progressTotalKnown
  const progress = safeProgress(complete, task.progressTotal)
  const displayTotal = Number.isFinite(task.progressTotal) ? Math.max(0, task.progressTotal) : 0

  return <li className="local-task-item" data-instance-id={task.instanceId}>
    <div className="local-task-item__identity" data-local-task-field="identity">
      <span>{modeLabel(task.mode)} / {createdAtLabel(task.createdAt)}</span>
      <span className="local-task-item__id" title={task.taskId}>{task.taskId}</span>
    </div>
    <div className="local-task-item__status" data-local-task-field="status">{statusLabelForTask(task.status)} · {phaseLabelForTask(task.phase)}</div>
    <div className="local-task-item__progress" data-local-task-field="progress">
      <span>进度</span>
      {knownProgress
        ? <progress aria-label="任务进度" value={progress.value} max={progress.max} />
        : <span className="local-task-item__progress-indeterminate" role="progressbar" aria-label="任务进度（总数未知）" aria-valuetext={`${complete} / --`}><span /></span>}
    </div>
    <div className="local-task-item__count" data-local-task-field="count">{complete} / {knownProgress ? displayTotal : '--'}</div>
    <div className="local-task-item__metrics" data-local-task-field="metrics">{task.speed || '—'} · 失败 {task.failures ?? 0} · {task.duration || '—'}</div>
    <div className="local-task-item__actions action-bar" data-local-task-field="actions" aria-label="任务操作">
      {actions.length > 0
        ? actions.map((operation) => <button key={operation} type="button" className="button-secondary" disabled={locked} onClick={() => onAction(operation, control)}>{actionLabel(operation, task.status)}</button>)
        : <button type="button" className="button-secondary" disabled aria-label="任务操作暂不可用">暂不可操作</button>}
    </div>
    {task.errorSummary && <p className="local-task-item__error" role="alert">{task.errorSummary}</p>}
  </li>
}
