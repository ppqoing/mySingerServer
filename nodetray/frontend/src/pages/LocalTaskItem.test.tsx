import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LocalTaskItem } from './LocalTaskItem'
import type { LocalTask } from './localTaskLifecycle'

const runningTask: LocalTask = {
  taskId: 'task-abcdef0123456789',
  instanceId: 'instance-new',
  revision: 7,
  source: 'D:\\Media',
  mode: 'scan_then_analysis',
  stage: 2,
  status: 'running',
  phase: 'stage2',
  roots: ['D:\\Media'],
  progressComplete: 40,
  progressTotal: 100,
  progressTotalKnown: true,
  speed: '12 文件/秒',
  failures: 2,
  duration: '00:03:12',
  syncStatus: '本机已保存',
  createdAt: 1_725_000_000_000,
  updatedAt: 1_725_000_010_000,
  startedAt: 1_725_000_001_000,
  completedAt: 0,
}

describe('LocalTaskItem', () => {
  it('完整显示任务 ID、进度和指标', () => {
    render(<LocalTaskItem task={runningTask} locked={false} onAction={vi.fn()} />)

    const item = screen.getByRole('listitem')
    expect(screen.getByTitle(runningTask.taskId)).toHaveTextContent(runningTask.taskId)
    expect(item).toHaveTextContent('扫描并自动一、二、三筛')
    expect(item).toHaveTextContent('运行中 · 二筛')
    expect(item).toHaveTextContent('进度')
    expect(item).toHaveTextContent('40 / 100')
    expect(item).toHaveTextContent('12 文件/秒 · 失败 2 · 00:03:12')
    expect(item.querySelectorAll('[data-local-task-field]')).toHaveLength(6)
    expect([...item.querySelectorAll('[data-local-task-field]')].map((node) => node.getAttribute('data-local-task-field')))
      .toEqual(['identity', 'status', 'progress', 'count', 'metrics', 'actions'])

    expect(screen.getByRole('progressbar')).toHaveAttribute('value', '40')
    expect(screen.getByRole('progressbar')).toHaveAttribute('max', '100')
  })

  it('将真实 scan_only 模式映射为中文', () => {
    render(<LocalTaskItem task={{ ...runningTask, mode: 'scan_only' }} locked={false} onAction={vi.fn()} />)

    expect(screen.getByRole('listitem')).toHaveTextContent('仅扫描')
    expect(screen.getByRole('listitem')).not.toHaveTextContent('scan_only')
  })

  it('为未知总数使用不确定进度条和占位总数', () => {
    render(<LocalTaskItem task={{ ...runningTask, progressComplete: 17, progressTotal: 0, progressTotalKnown: false }} locked={false} onAction={vi.fn()} />)

    expect(screen.getByRole('progressbar', { name: '任务进度（总数未知）' })).toHaveAttribute('aria-valuetext', '17 / --')
    expect(screen.getByText('17 / --')).toBeVisible()
  })

  it('在已知总数为零时保持有效原生 progress 属性', () => {
    render(<LocalTaskItem task={{ ...runningTask, progressComplete: 0, progressTotal: 0 }} locked={false} onAction={vi.fn()} />)

    expect(screen.getByText('0 / 0')).toBeVisible()
    expect(screen.getByRole('progressbar')).toHaveAttribute('value', '0')
    expect(screen.getByRole('progressbar')).toHaveAttribute('max', '1')
  })

  it.each([
    ['pausing', '正在暂停'],
    ['stopping', '正在停止'],
    ['deleting', '正在删除'],
  ] as const)('为过渡状态 %s 显示禁用控制', (status, label) => {
    render(<LocalTaskItem task={{ ...runningTask, status }} locked={false} onAction={vi.fn()} />)

    expect(screen.getByText(label, { exact: false })).toBeVisible()
    expect(screen.getByRole('button', { name: '任务操作暂不可用' })).toBeDisabled()
  })

  it('保留当前行的安全错误并以实例和版本回调操作', async () => {
    const onAction = vi.fn()
    render(<LocalTaskItem task={{ ...runningTask, errorCode: 'revision_conflict', errorSummary: '任务状态已更新，请重试' }} locked={false} onAction={onAction} />)
    const user = userEvent.setup()

    expect(screen.getByRole('alert')).toHaveTextContent('任务状态已更新，请重试')
    await user.click(screen.getByRole('button', { name: '暂停' }))
    expect(onAction).toHaveBeenCalledWith('pause', {
      taskId: runningTask.taskId,
      instanceId: runningTask.instanceId,
      expectedRevision: runningTask.revision,
    })
  })

  it('将删除失败显示为重试删除，并在锁定时禁用操作', () => {
    render(<LocalTaskItem task={{ ...runningTask, status: 'delete_failed' }} locked onAction={vi.fn()} />)

    expect(screen.getByRole('button', { name: '重试删除' })).toBeDisabled()
    expect(screen.queryByRole('button', { name: '删除' })).not.toBeInTheDocument()
  })
})
