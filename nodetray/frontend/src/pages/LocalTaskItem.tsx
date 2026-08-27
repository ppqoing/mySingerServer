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

type LocalTaskIO = {
  diskConcurrency: number
  effectiveReadBps: number
  leaseWaitMs: number
  sequentialBytes: number
  seekCount: number
  busyWorkers: number
  ioWaitWorkers: number
}

const positive = (value: number | undefined): value is number => Number.isFinite(value) && (value ?? 0) > 0
const compact = (value: number): string => Number.isInteger(value) ? String(value) : value.toFixed(1).replace(/\.0$/, '')
const formatCount = (value: number | undefined): string => positive(value) ? String(Math.trunc(value)) : '—'
const formatMilliseconds = (value: number | undefined): string => positive(value) ? `${compact(value)} ms` : '—'
const formatBytes = (value: number | undefined, perSecond = false): string => {
  if (!positive(value)) return '—'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let scaled = value
  let unit = 0
  while (scaled >= 1024 && unit < units.length - 1) {
    scaled /= 1024
    unit += 1
  }
  return `${compact(scaled)} ${units[unit]}${perSecond ? '/s' : ''}`
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
  const io = (task as LocalTask & { io?: Partial<LocalTaskIO> }).io ?? {}

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
    <details className="local-task-item__io">
      <summary>磁盘 I/O 详情</summary>
      <dl>
        <div><dt>磁盘并发</dt><dd>{formatCount(io.diskConcurrency)}</dd></div>
        <div><dt>有效读取速度</dt><dd>{formatBytes(io.effectiveReadBps, true)}</dd></div>
        <div><dt>租约等待</dt><dd>{formatMilliseconds(io.leaseWaitMs)}</dd></div>
        <div><dt>顺序字节</dt><dd>{formatBytes(io.sequentialBytes)}</dd></div>
        <div><dt>Seek</dt><dd>{formatCount(io.seekCount)}</dd></div>
        <div><dt>忙 Worker</dt><dd>{formatCount(io.busyWorkers)}</dd></div>
        <div><dt>I/O 等待 Worker</dt><dd>{formatCount(io.ioWaitWorkers)}</dd></div>
      </dl>
    </details>
    <div className="local-task-item__actions action-bar" data-local-task-field="actions" aria-label="任务操作">
      {actions.length > 0
        ? actions.map((operation) => <button key={operation} type="button" className="button-secondary" disabled={locked} onClick={() => onAction(operation, control)}>{actionLabel(operation, task.status)}</button>)
        : <button type="button" className="button-secondary" disabled aria-label="任务操作暂不可用">暂不可操作</button>}
    </div>
    {task.errorSummary && <p className="local-task-item__error" role="alert">{task.errorSummary}</p>}
  </li>
}
