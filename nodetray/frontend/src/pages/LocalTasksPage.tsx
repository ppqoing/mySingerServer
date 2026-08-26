import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { chooseLocalTaskRoot, createLocalTask, listLocalTasks, type LocalTask, type LocalTaskCreate, type LocalTaskPage, type PathSelectionResult } from '../api/localAgent'
import { planTaskRootAddition } from './taskRoots'

export type LocalTasksAPI = {
  choose: (currentPath: string) => Promise<PathSelectionResult>
  create: (request: LocalTaskCreate) => Promise<unknown>
  list: () => Promise<LocalTaskPage>
}

const defaultAPI: LocalTasksAPI = { choose: chooseLocalTaskRoot, create: createLocalTask, list: listLocalTasks }

const terminalStatuses = new Set(['succeeded', 'failed', 'cancelled'])

const statusText: Record<string, string> = {
  pending: '等待中',
  running: '运行中',
  waiting_recovery: '等待恢复',
  succeeded: '已完成',
  failed: '失败',
  cancelled: '已取消',
}

const stageText = (stage: number): string => (stage > 0 ? `阶段 ${stage}` : '')

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

  const refresh = useCallback(async (): Promise<void> => {
    const page = await api.list()
    if (page.ok) setTasks(page.tasks)
  }, [api])
  useEffect(() => { void api.list().then((page) => { if (page.ok) setTasks(page.tasks) }) }, [api])
  // 有非终态任务时每 2 秒轮询进度；全部终态后停止。
  useEffect(() => {
    if (!tasks.some((task) => !terminalStatuses.has(task.status))) return undefined
    const timer = setInterval(() => { void refresh() }, 2000)
    return () => clearInterval(timer)
  }, [tasks, refresh])

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
    const result = await api.choose(roots.at(-1) ?? '')
    if (!result.ok) { setMessage(result.errorSummary ?? '无法打开目录选择窗口'); return }
    if (result.cancelled) return
    await addRoot(result.path)
  }
  const submit = async (): Promise<void> => {
    if (!roots.length) { setMessage('请选择扫描目录'); return }
    const request = { taskId: `local-${Date.now()}`, roots, mode, rescan: false, extensions: [] }
    const result = (await api.create(request)) as { ok?: boolean; errorSummary?: string }
    if (result && result.ok === false) { setMessage(result.errorSummary ?? '创建任务失败'); return }
    setMessage('任务已提交')
    await refresh()
  }

  return <section className="local-tasks"><h2>本地任务</h2>
    <div className="local-tasks__root-actions"><label>手工目录<input aria-label="手工目录" value={manualRoot} onChange={(event) => setManualRoot(event.target.value)} /></label><button type="button" onClick={() => void addRoot(manualRoot)}>添加目录</button><button type="button" onClick={() => void chooseRoot()}>选择目录…</button></div>
    <p className="local-tasks__help">原生目录窗口每次选择一个目录；重复选择即可添加多个目录。隐藏项显示跟随 Windows 资源管理器设置。</p>
    <ul className="local-tasks__roots" aria-label="扫描目录列表">{roots.map((root) => <li key={root}><span>{root}</span><button type="button" aria-label={`移除 ${root}`} onClick={() => setRoots((current) => current.filter((item) => item !== root))}>移除</button></li>)}</ul>
    <label>任务模式<select aria-label="任务模式" value={mode} onChange={(event) => setMode(event.target.value)}><option value="scan_only">仅扫描</option><option value="scan_then_analysis">扫描并自动一、二、三筛</option></select></label><p>一筛基础特征为默认计算项，不能关闭。</p><button type="button" onClick={() => void submit()}>创建任务</button><p role="status">{message}</p>
    {tasks.length === 0 ? <p className="local-tasks__empty">暂无本地任务。</p> : (
      <ul className="local-tasks__list" aria-label="任务列表">{tasks.map((task) => {
        const complete = task.progressComplete ?? 0
        const total = task.progressTotal ?? 0
        return <li key={task.taskId}>
          <div><strong>{task.taskId}</strong> · {statusText[task.status] ?? task.status}{stageText(task.stage) ? ` · ${stageText(task.stage)}` : ''}</div>
          <div><progress max={total} value={complete} /> <span>{complete}/{total}</span></div>
          <div>
            {task.speed ? <span>速度 {task.speed} · </span> : null}
            {task.failures ? <span>失败 {task.failures} · </span> : null}
            {task.duration ? <span>耗时 {task.duration} · </span> : null}
            {task.syncStatus ? <span>同步 {task.syncStatus}</span> : null}
          </div>
          {task.errorSummary ? <p role="alert">{task.errorSummary}</p> : null}
        </li>
      })}</ul>
    )}
  </section>
}
