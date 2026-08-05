import type { ReactNode } from 'react'
import { sanitizeSummary, type WorkerState } from '../state/nodeStore'

type WorkerSummaryProps = {
  ready: number
  expected: number
  workers: WorkerState[]
}

export function WorkerSummary({ ready, expected, workers }: WorkerSummaryProps): ReactNode {
  return (
    <section aria-labelledby="worker-summary-title">
      <h3 id="worker-summary-title">Worker 状态</h3>
      <p>
        就绪：<strong>{ready} / {expected}</strong>
      </p>
      <table>
        <thead>
          <tr>
            <th scope="col">Worker</th>
            <th scope="col">PID</th>
            <th scope="col">就绪</th>
            <th scope="col">当前任务</th>
            <th scope="col">最近异常</th>
          </tr>
        </thead>
        <tbody>
          {workers.length === 0 ? (
            <tr>
              <td colSpan={5}>暂无 Worker 状态</td>
            </tr>
          ) : (
            workers.map((worker) => (
              <tr key={worker.index}>
                <th scope="row">Worker {worker.index + 1}</th>
                <td>{worker.pid > 0 ? worker.pid : '—'}</td>
                <td>{worker.ready ? '已就绪' : '未就绪'}</td>
                <td>{sanitizeSummary(worker.currentTaskSummary) || '—'}</td>
                <td>{sanitizeSummary(worker.lastErrorSummary) || '—'}</td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </section>
  )
}
