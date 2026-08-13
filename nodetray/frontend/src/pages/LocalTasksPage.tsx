import { useEffect, useState, type ReactNode } from 'react'
import { chooseLocalTaskRoot, createLocalTask, listLocalTasks, type LocalTaskCreate, type LocalTaskPage, type PathSelectionResult } from '../api/localAgent'
import { planTaskRootAddition } from './taskRoots'

export type LocalTasksAPI = {
  choose: (currentPath: string) => Promise<PathSelectionResult>
  create: (request: LocalTaskCreate) => Promise<unknown>
  list: () => Promise<LocalTaskPage>
}

const defaultAPI: LocalTasksAPI = { choose: chooseLocalTaskRoot, create: createLocalTask, list: listLocalTasks }

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
  const [tasks, setTasks] = useState<LocalTaskPage['tasks']>([])
  useEffect(() => { void api.list().then((page) => page.ok && setTasks(page.tasks)) }, [api])

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
    await api.create(request)
    setMessage('任务已提交')
  }

  return <section className="local-tasks"><h2>本地任务</h2>
    <div className="local-tasks__root-actions"><label>手工目录<input aria-label="手工目录" value={manualRoot} onChange={(event) => setManualRoot(event.target.value)} /></label><button type="button" onClick={() => void addRoot(manualRoot)}>添加目录</button><button type="button" onClick={() => void chooseRoot()}>选择目录…</button></div>
    <p className="local-tasks__help">原生目录窗口每次选择一个目录；重复选择即可添加多个目录。隐藏项显示跟随 Windows 资源管理器设置。</p>
    <ul className="local-tasks__roots" aria-label="扫描目录列表">{roots.map((root) => <li key={root}><span>{root}</span><button type="button" aria-label={`移除 ${root}`} onClick={() => setRoots((current) => current.filter((item) => item !== root))}>移除</button></li>)}</ul>
    <label>任务模式<select aria-label="任务模式" value={mode} onChange={(event) => setMode(event.target.value)}><option value="scan_only">仅扫描</option><option value="scan_then_analysis">扫描并自动一、二、三筛</option></select></label><p>一筛基础特征为默认计算项，不能关闭。</p><button type="button" onClick={() => void submit()}>创建任务</button><p role="status">{message}</p><ul>{tasks.map((task) => <li key={task.taskId}>{task.taskId} · {task.status}</li>)}</ul>
  </section>
}
