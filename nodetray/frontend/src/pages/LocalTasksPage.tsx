import { useEffect, useRef, useState, type ReactNode } from 'react'
import {
  cancelLocalTask,
  chooseLocalTaskRoot,
  createLocalTask,
  deleteLocalTask,
  listLocalTasks,
  pauseLocalTask,
  resumeLocalTask,
  retryLocalTask,
  type LocalTask,
  type LocalTaskControl,
  type LocalTaskCreate,
  type LocalTaskPage,
  type LocalTaskResult,
  type PathSelectionResult,
} from '../api/localAgent'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { LocalTaskItem } from './LocalTaskItem'
import { isActiveLocalTaskStatus, type LocalTaskOperation } from './localTaskLifecycle'
import { planTaskRootAddition } from './taskRoots'

type RequestGeneration = number

interface OperationLock {
  apiGeneration: RequestGeneration
  instanceId: string
  revision: number
  operation: LocalTaskOperation
}

type PendingConfirmation = {
  operation: 'cancel' | 'delete'
  control: LocalTaskControl
}

const ACTIVE_POLL_MS = 1_000
const IDLE_POLL_MS = 5_000
const LIST_REQUEST_TIMEOUT_MS = 10_000

export type LocalTasksAPI = {
  choose: (currentPath: string) => Promise<PathSelectionResult>
  create: (request: LocalTaskCreate) => Promise<LocalTaskResult>
  list: () => Promise<LocalTaskPage>
  pause: (control: LocalTaskControl) => Promise<LocalTaskResult>
  resume: (control: LocalTaskControl) => Promise<LocalTaskResult>
  cancel: (control: LocalTaskControl) => Promise<LocalTaskResult>
  delete: (control: LocalTaskControl) => Promise<LocalTaskResult>
  retry: (control: LocalTaskControl) => Promise<LocalTaskResult>
}

const defaultAPI: LocalTasksAPI = {
  choose: chooseLocalTaskRoot,
  create: createLocalTask,
  list: listLocalTasks,
  pause: pauseLocalTask,
  resume: resumeLocalTask,
  cancel: cancelLocalTask,
  delete: deleteLocalTask,
  retry: retryLocalTask,
}

const compareNewest = (left: LocalTask, right: LocalTask): number =>
  right.createdAt - left.createdAt
  || right.updatedAt - left.updatedAt
  || right.revision - left.revision
  || right.instanceId.localeCompare(left.instanceId)

const newestFirst = (tasks: LocalTask[]): LocalTask[] => {
  const result: LocalTask[] = []
  const seenTaskIDs = new Set<string>()
  for (const task of [...tasks].sort(compareNewest)) {
    if (seenTaskIDs.has(task.taskId)) continue
    seenTaskIDs.add(task.taskId)
    result.push(task)
  }
  return result
}

const mergeAuthoritativeList = (current: LocalTask[], incoming: LocalTask[]): LocalTask[] => newestFirst(incoming).map((candidate) => {
  const existing = current.find((task) => task.taskId === candidate.taskId)
  return existing?.instanceId === candidate.instanceId && existing.revision > candidate.revision
    ? existing
    : candidate
}).sort(compareNewest)

const upsertCreatedTask = (tasks: LocalTask[], incoming: LocalTask, allowInstanceReplacement: boolean): LocalTask[] => {
  const current = tasks.find((task) => task.taskId === incoming.taskId)
  if (current?.instanceId === incoming.instanceId && current.revision > incoming.revision) return tasks
  if (current?.instanceId !== undefined && current.instanceId !== incoming.instanceId && !allowInstanceReplacement) return tasks
  return newestFirst([incoming, ...tasks.filter((task) => task.taskId !== incoming.taskId)])
}

const sameLock = (left: OperationLock | undefined, right: OperationLock): boolean =>
  left?.apiGeneration === right.apiGeneration
  && left.instanceId === right.instanceId
  && left.revision === right.revision
  && left.operation === right.operation

export function LocalTasksPage({
  api = defaultAPI,
  confirmReplace = async () => window.confirm('新目录包含已选子目录，是否替换？'),
}: {
  api?: LocalTasksAPI
  confirmReplace?: (coveredRoots: string[], parentRoot: string) => Promise<boolean>
}): ReactNode {
  const [roots, setRoots] = useState<string[]>([])
  const [manualRoot, setManualRoot] = useState('')
  const [mode, setMode] = useState('scan_then_analysis')
  const [message, setMessage] = useState('')
  const [tasks, setTasks] = useState<LocalTask[]>([])
  const [stale, setStale] = useState(false)
  const [creating, setCreating] = useState(false)
  const [locks, setLocks] = useState<Map<string, OperationLock>>(() => new Map())
  const [confirmation, setConfirmation] = useState<PendingConfirmation>()

  const apiRef = useRef(api)
  const apiGenerationRef = useRef<RequestGeneration>(0)
  const listSequenceRef = useRef(0)
  const acceptedListSequenceRef = useRef(0)
  const tasksRef = useRef<LocalTask[]>([])
  const timerRef = useRef<number | undefined>(undefined)
  const refreshRef = useRef<() => void>(() => undefined)
  const locksRef = useRef<Map<string, OperationLock>>(new Map())

  useEffect(() => {
    const apiGeneration = ++apiGenerationRef.current
    apiRef.current = api
    locksRef.current = new Map()
    queueMicrotask(() => {
      if (apiGeneration !== apiGenerationRef.current) return
      setLocks(new Map())
      setConfirmation(undefined)
      setCreating(false)
    })
    window.clearTimeout(timerRef.current)

    const scheduleNext = (delay: number): void => {
      if (apiGeneration !== apiGenerationRef.current) return
      window.clearTimeout(timerRef.current)
      timerRef.current = window.setTimeout(() => void poll(), delay)
    }

    const poll = async (): Promise<void> => {
      window.clearTimeout(timerRef.current)
      timerRef.current = undefined
      const requestSequence = ++listSequenceRef.current
      let requestTimeout: number | undefined
      try {
        const timeout = new Promise<never>((_, reject) => {
          requestTimeout = window.setTimeout(() => reject(new Error('local task list timeout')), LIST_REQUEST_TIMEOUT_MS)
          timerRef.current = requestTimeout
        })
        const result = await Promise.race([api.list(), timeout])
        if (apiGeneration !== apiGenerationRef.current || requestSequence !== listSequenceRef.current) return
        if (!result.ok) {
          setStale(true)
          scheduleNext(IDLE_POLL_MS)
          return
        }
        const incoming = mergeAuthoritativeList(tasksRef.current, result.tasks)
        acceptedListSequenceRef.current = requestSequence
        tasksRef.current = incoming
        setTasks(incoming)
        setStale(false)
        scheduleNext(incoming.some((task) => isActiveLocalTaskStatus(task.status)) ? ACTIVE_POLL_MS : IDLE_POLL_MS)
      } catch {
        if (apiGeneration !== apiGenerationRef.current || requestSequence !== listSequenceRef.current) return
        setStale(true)
        scheduleNext(IDLE_POLL_MS)
      } finally {
        window.clearTimeout(requestTimeout)
        if (timerRef.current === requestTimeout) timerRef.current = undefined
      }
    }

    refreshRef.current = () => void poll()
    void poll()

    return () => {
      apiGenerationRef.current = apiGeneration + 1
      window.clearTimeout(timerRef.current)
      timerRef.current = undefined
      refreshRef.current = () => undefined
    }
  }, [api])

  const addRoot = async (value: string): Promise<void> => {
    const plan = planTaskRootAddition(roots, value)
    if (plan.kind === 'invalid') { setMessage('请输入有效的绝对 Windows 或 UNC 目录'); return }
    if (plan.kind === 'duplicate') { setMessage('该目录已在列表中'); return }
    if (plan.kind === 'covered') { setMessage('该目录已被父目录覆盖'); return }
    if (plan.kind === 'replace-children' && !(await confirmReplace(plan.coveredRoots, plan.roots.at(-1) ?? value))) return
    setRoots(plan.roots)
    setManualRoot('')
    setMessage('')
  }

  const chooseRoot = async (): Promise<void> => {
    const apiGeneration = apiGenerationRef.current
    const result = await apiRef.current.choose(roots.at(-1) ?? '')
    if (apiGeneration !== apiGenerationRef.current) return
    if (!result.ok) { setMessage(result.errorSummary ?? '无法打开目录选择窗口'); return }
    if (result.cancelled) return
    await addRoot(result.path)
  }

  const submit = async (): Promise<void> => {
    if (!roots.length) { setMessage('请选择扫描目录'); return }
    const apiGeneration = apiGenerationRef.current
    const acceptedListAtStart = acceptedListSequenceRef.current
    const request = { taskId: `local-${Date.now()}`, roots: [...roots], mode, rescan: false, extensions: [] }
    setCreating(true)
    try {
      const result = await apiRef.current.create(request)
      if (apiGeneration !== apiGenerationRef.current) return
      if (!result.ok || !result.task) {
        setMessage(result.errorSummary ?? '创建任务失败')
        return
      }
      const nextTasks = upsertCreatedTask(
        tasksRef.current,
        result.task,
        acceptedListSequenceRef.current <= acceptedListAtStart,
      )
      tasksRef.current = nextTasks
      setTasks(nextTasks)
      setRoots([])
      setManualRoot('')
      setMode('scan_then_analysis')
      setMessage('任务已提交')
      refreshRef.current()
    } catch {
      if (apiGeneration === apiGenerationRef.current) setMessage('创建任务失败')
    } finally {
      if (apiGeneration === apiGenerationRef.current) setCreating(false)
    }
  }

  const installLock = (lock: OperationLock): void => {
    const next = new Map(locksRef.current)
    next.set(lock.instanceId, lock)
    locksRef.current = next
    setLocks(next)
  }

  const releaseLock = (lock: OperationLock): void => {
    if (!sameLock(locksRef.current.get(lock.instanceId), lock)) return
    const next = new Map(locksRef.current)
    next.delete(lock.instanceId)
    locksRef.current = next
    setLocks(next)
  }

  const setItemError = (control: LocalTaskControl, result: Pick<LocalTaskResult, 'errorCode' | 'errorSummary'>): void => {
    const nextTasks = tasksRef.current.map((task) =>
      task.taskId === control.taskId
      && task.instanceId === control.instanceId
      && task.revision === control.expectedRevision
        ? { ...task, errorCode: result.errorCode, errorSummary: result.errorSummary ?? '任务操作失败' }
        : task,
    )
    tasksRef.current = nextTasks
    setTasks(nextTasks)
  }

  const runOperation = async (operation: LocalTaskOperation, control: LocalTaskControl): Promise<void> => {
    const apiGeneration = apiGenerationRef.current
    const lock: OperationLock = {
      apiGeneration,
      instanceId: control.instanceId,
      revision: control.expectedRevision,
      operation,
    }
    installLock(lock)
    try {
      const result = await apiRef.current[operation](control)
      if (apiGeneration !== apiGenerationRef.current || !sameLock(locksRef.current.get(lock.instanceId), lock)) return
      if (!result.ok) {
        if (result.errorCode !== 'stale_task' && result.errorCode !== 'task_instance_mismatch') setItemError(control, result)
        releaseLock(lock)
        if (result.errorCode === 'stale_task' || result.errorCode === 'task_instance_mismatch') refreshRef.current()
        return
      }
      if (result.deleted) {
        const nextTasks = tasksRef.current.filter((task) =>
          task.taskId !== control.taskId
          || task.instanceId !== control.instanceId
          || task.revision !== control.expectedRevision,
        )
        tasksRef.current = nextTasks
        setTasks(nextTasks)
      } else if (result.task) {
        const nextTasks = tasksRef.current.map((task) =>
          task.taskId === control.taskId
          && task.instanceId === control.instanceId
          && result.task!.taskId === control.taskId
          && result.task!.instanceId === control.instanceId
          && result.task!.revision >= task.revision
            ? result.task!
            : task,
        )
        tasksRef.current = nextTasks
        setTasks(nextTasks)
      }
      releaseLock(lock)
      refreshRef.current()
    } catch {
      if (apiGeneration !== apiGenerationRef.current || !sameLock(locksRef.current.get(lock.instanceId), lock)) return
      setItemError(control, { errorCode: 'request_failed', errorSummary: '任务操作失败，请重试' })
      releaseLock(lock)
    }
  }

  const onAction = (operation: LocalTaskOperation, control: LocalTaskControl): void => {
    if (operation === 'cancel' || operation === 'delete') {
      setConfirmation({ operation, control })
      return
    }
    void runOperation(operation, control)
  }

  const confirmAction = (): void => {
    if (!confirmation) return
    const pending = confirmation
    setConfirmation(undefined)
    void runOperation(pending.operation, pending.control)
  }

  const deleting = confirmation?.operation === 'delete'

  return <section className="local-tasks"><h2>本地任务</h2>
    <div className="local-tasks__root-actions"><label>手工目录<input aria-label="手工目录" value={manualRoot} onChange={(event) => setManualRoot(event.target.value)} /></label><button type="button" onClick={() => void addRoot(manualRoot)}>添加目录</button><button type="button" onClick={() => void chooseRoot()}>选择目录…</button></div>
    <p className="local-tasks__help">原生目录窗口每次选择一个目录；重复选择即可添加多个目录。隐藏项显示跟随 Windows 资源管理器设置。</p>
    <ul className="local-tasks__roots" aria-label="扫描目录列表">{roots.map((root) => <li key={root}><span>{root}</span><button type="button" aria-label={`移除 ${root}`} onClick={() => setRoots((current) => current.filter((item) => item !== root))}>移除</button></li>)}</ul>
    <label>任务模式<select aria-label="任务模式" value={mode} onChange={(event) => setMode(event.target.value)}><option value="scan_only">仅扫描</option><option value="scan_then_analysis">扫描并自动一、二、三筛</option></select></label><p>一筛基础特征为默认计算项，不能关闭。</p><button type="button" disabled={creating} onClick={() => void submit()}>创建任务</button><p role="status">{message}</p>
    {stale && <p className="local-tasks__stale">状态可能已过期，正在重试。</p>}
    <ul aria-label="本地任务列表">{tasks.map((task) => {
      const lock = locks.get(task.instanceId)
      const locked = lock?.revision === task.revision
      return <LocalTaskItem key={task.instanceId} task={task} locked={locked} onAction={onAction} />
    })}</ul>
    <ConfirmDialog
      open={confirmation !== undefined}
      title={deleting ? '删除任务' : '停止任务'}
      description={deleting
        ? <span>删除本机任务及其分析、分组、评分和审核数据；保留文件、全局索引、特征与缓存；保留文件删除审计及其同步记录；不撤回已同步的中央数据。</span>
        : <span>停止后不再继续处理；已完成结果会保留。</span>}
      confirmLabel={deleting ? '确认删除' : '确认停止'}
      onConfirm={confirmAction}
      onCancel={() => setConfirmation(undefined)}
    />
  </section>
}
