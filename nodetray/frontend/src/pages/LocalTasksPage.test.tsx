import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LocalTasksPage } from './LocalTasksPage'

it('创建扫描或自动三筛任务且一筛特征固定启用', async () => {
  const create = vi.fn(async () => ({ ok: true, task: { taskId: 't1' } }))
  render(<LocalTasksPage api={{ create, list: vi.fn(async () => ({ ok: true, tasks: [] })) }} />)
  expect(screen.getByText(/一筛基础特征.*默认计算/)).toBeVisible()
  expect(screen.queryByRole('checkbox', { name: /一筛/ })).not.toBeInTheDocument()
  await userEvent.type(screen.getByLabelText('扫描目录'), 'D:\\media')
  await userEvent.selectOptions(screen.getByLabelText('任务模式'), 'scan_then_analysis')
  await userEvent.click(screen.getByRole('button', { name: '创建任务' }))
  expect(create).toHaveBeenCalledWith(expect.objectContaining({ roots: ['D:\\media'], mode: 'scan_then_analysis' }))
})
