export type LocalTaskStatus =
  | 'pending' | 'running' | 'waiting_recovery' | 'pausing' | 'paused'
  | 'stopping' | 'cancelled' | 'succeeded' | 'failed'
  | 'deleting' | 'delete_failed'

export type LocalTaskPhase =
  | 'waiting' | 'scan' | 'stage1' | 'stage2' | 'stage3' | 'finalizing'

export interface LocalTask {
  taskId: string
  instanceId: string
  revision: number
  source: string
  mode: string
  stage: number
  status: LocalTaskStatus
  phase: LocalTaskPhase
  roots: string[]
  progressComplete: number
  progressTotal: number
  progressTotalKnown: boolean
  speed: string
  failures: number
  duration: string
  syncStatus: string
  errorCode?: string
  errorSummary?: string
  createdAt: number
  updatedAt: number
  startedAt: number
  completedAt: number
}

export type LocalTaskOperation = 'pause' | 'resume' | 'cancel' | 'delete' | 'retry'

export interface LocalTaskControl {
  taskId: string
  instanceId: string
  expectedRevision: number
}

export interface LocalTaskResult {
  ok: boolean
  task?: LocalTask
  deleted?: boolean
  errorCode?: string
  errorSummary?: string
}

const actionsByStatus: Record<LocalTaskStatus, readonly LocalTaskOperation[]> = {
  pending: ['pause', 'cancel', 'delete'],
  running: ['pause', 'cancel', 'delete'],
  waiting_recovery: ['pause', 'cancel', 'delete'],
  pausing: [],
  paused: ['resume', 'cancel', 'delete'],
  stopping: [],
  cancelled: ['retry', 'delete'],
  succeeded: ['delete'],
  failed: ['retry', 'delete'],
  deleting: [],
  delete_failed: ['delete'],
}

const statusLabel: Record<LocalTaskStatus, string> = {
  pending: '等待中',
  running: '运行中',
  waiting_recovery: '等待恢复',
  pausing: '正在暂停',
  paused: '已暂停',
  stopping: '正在停止',
  cancelled: '已停止',
  succeeded: '已完成',
  failed: '失败',
  deleting: '正在删除',
  delete_failed: '删除失败',
}

const phaseLabel: Record<LocalTaskPhase, string> = {
  waiting: '等待',
  scan: '枚举与扫描',
  stage1: '一筛',
  stage2: '二筛',
  stage3: '三筛',
  finalizing: '安全收尾',
}

const activeStatuses = new Set<LocalTaskStatus>([
  'pending', 'running', 'waiting_recovery', 'pausing', 'stopping', 'deleting',
])

export function actionsForTaskStatus(status: string): readonly LocalTaskOperation[] {
  return actionsByStatus[status as LocalTaskStatus] ?? []
}

export function statusLabelForTask(status: string): string {
  return statusLabel[status as LocalTaskStatus] ?? '未知状态'
}

export function phaseLabelForTask(phase: string): string {
  return phaseLabel[phase as LocalTaskPhase] ?? '未知阶段'
}

export function isActiveLocalTaskStatus(status: string): boolean {
  return activeStatuses.has(status as LocalTaskStatus)
}
