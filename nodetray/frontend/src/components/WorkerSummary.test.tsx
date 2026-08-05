import '@testing-library/jest-dom/vitest'
import { render, screen, within } from '@testing-library/react'
import { WorkerSummary } from './WorkerSummary'

describe('WorkerSummary', () => {
  it('显示 Worker 就绪计数、PID、任务和最近异常', () => {
    render(
      <WorkerSummary
        ready={1}
        expected={2}
        workers={[
          { index: 0, pid: 2101, ready: true, currentTaskSummary: '等待任务', lastErrorSummary: '' },
          { index: 1, pid: 2102, ready: false, currentTaskSummary: '处理队列', lastErrorSummary: '任务超时' },
        ]}
      />,
    )

    expect(screen.getByRole('heading', { name: 'Worker 状态' })).toBeVisible()
    expect(screen.getByText('1 / 2')).toBeVisible()
    const secondRow = screen.getByRole('row', { name: /Worker 2/ })
    expect(secondRow).toHaveTextContent('2102')
    expect(secondRow).toHaveTextContent('未就绪')
    expect(secondRow).toHaveTextContent('处理队列')
    expect(secondRow).toHaveTextContent('任务超时')
  })

  it('Worker 区域没有动作角色或任何 Worker 管理按钮', () => {
    render(
      <WorkerSummary
        ready={0}
        expected={1}
        workers={[
          { index: 0, pid: 0, ready: false, currentTaskSummary: '无', lastErrorSummary: '无' },
        ]}
      />,
    )

    const region = screen.getByRole('region', { name: 'Worker 状态' })
    expect(within(region).queryByRole('button')).not.toBeInTheDocument()
    expect(within(region).queryByRole('link')).not.toBeInTheDocument()
    expect(within(region).queryByRole('menuitem')).not.toBeInTheDocument()
    expect(within(region).queryByRole('group')).not.toBeInTheDocument()
    for (const label of ['启动 Worker', '停止 Worker', '重启 Worker', '删除 Worker']) {
      expect(within(region).queryByText(label)).not.toBeInTheDocument()
    }
  })
})
