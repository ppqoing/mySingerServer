import { useEffect, useState, type ReactNode } from 'react'
import { createLocalTask, listLocalTasks, type LocalTaskCreate, type LocalTaskPage } from '../api/localAgent'

type API = { create: (request: LocalTaskCreate) => Promise<unknown>; list: () => Promise<LocalTaskPage> }
const defaultAPI: API = { create: createLocalTask, list: listLocalTasks }

export function LocalTasksPage({ api = defaultAPI }: { api?: API }): ReactNode {
  const [root, setRoot] = useState('')
  const [mode, setMode] = useState('scan_then_analysis')
  const [message, setMessage] = useState('')
  const [tasks, setTasks] = useState<LocalTaskPage['tasks']>([])
  useEffect(() => { void api.list().then((page) => page.ok && setTasks(page.tasks)) }, [api])
  const submit = async (): Promise<void> => {
    if (!root.trim()) { setMessage('请选择扫描目录'); return }
    const request = { taskId: `local-${Date.now()}`, roots: [root.trim()], mode, rescan: false, extensions: [] }
    await api.create(request); setMessage('任务已提交')
  }
  return <section><h2>本地任务</h2><label>扫描目录<input aria-label="扫描目录" value={root} onChange={(event) => setRoot(event.target.value)} /></label><label>任务模式<select aria-label="任务模式" value={mode} onChange={(event) => setMode(event.target.value)}><option value="scan_only">仅扫描</option><option value="scan_then_analysis">扫描并自动一、二、三筛</option></select></label><p>一筛基础特征为默认计算项，不能关闭。</p><button type="button" onClick={() => void submit()}>创建任务</button><p role="status">{message}</p><ul>{tasks.map((task) => <li key={task.taskId}>{task.taskId} · {task.status}</li>)}</ul></section>
}
