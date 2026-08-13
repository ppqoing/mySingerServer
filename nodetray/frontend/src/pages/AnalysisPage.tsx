import { useEffect, useState, type ReactNode } from 'react'
import { listLocalTasks, type LocalTaskPage } from '../api/localAgent'

type API = { list: () => Promise<LocalTaskPage> }
export function AnalysisPage({ api = { list: listLocalTasks } }: { api?: API }): ReactNode {
  const [page, setPage] = useState<LocalTaskPage>({ ok: true, tasks: [] })
  useEffect(() => { void api.list().then(setPage) }, [api])
  return <section><h2>去重分析</h2>{!page.ok && <p>本机操作仍可用；同步暂不可用</p>}<table><thead><tr><th>来源</th><th>阶段</th><th>状态</th><th>速度</th><th>失败数</th><th>耗时</th><th>同步状态</th></tr></thead><tbody>{page.tasks.map((task) => <tr key={task.taskId}><td>{task.source}</td><td>{task.stage}</td><td>{task.status}</td><td>{task.speed || '—'}</td><td>{task.failures ?? 0}</td><td>{task.duration || '—'}</td><td>{task.syncStatus || '本机已保存'}</td></tr>)}</tbody></table></section>
}
