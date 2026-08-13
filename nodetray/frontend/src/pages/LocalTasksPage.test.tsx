import '@testing-library/jest-dom/vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LocalTasksPage } from './LocalTasksPage'
import type { LocalTasksAPI } from './LocalTasksPage'

const path = (value: string, cancelled = false) => ({ ok: true, path: value, cancelled })
const api = (overrides: Partial<LocalTasksAPI> = {}): LocalTasksAPI => ({
  choose: vi.fn(async () => path('')),
  create: vi.fn(async () => ({ ok: true, task: { taskId: 't1' } })),
  list: vi.fn(async () => ({ ok: true, tasks: [] })),
  ...overrides,
})

it('重复打开原生窗口添加多个目录后提交', async () => {
  const choose = vi.fn()
    .mockResolvedValueOnce(path('D:\\Media'))
    .mockResolvedValueOnce(path('E:\\Photos'))
  const create = vi.fn(async () => ({ ok: true, task: { taskId: 't1' } }))
  render(<LocalTasksPage api={api({ choose, create })} />)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '选择目录…' }))
  await user.click(screen.getByRole('button', { name: '选择目录…' }))
  await user.click(screen.getByRole('button', { name: '创建任务' }))
  expect(create).toHaveBeenCalledWith(expect.objectContaining({ roots: ['D:\\Media', 'E:\\Photos'] }))
})

it('取消原生窗口不改变已有目录列表', async () => {
  const choose = vi.fn().mockResolvedValueOnce(path('D:\\Media')).mockResolvedValueOnce(path('', true))
  render(<LocalTasksPage api={api({ choose })} />)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '选择目录…' }))
  await user.click(screen.getByRole('button', { name: '选择目录…' }))
  expect(screen.getByText('D:\\Media')).toBeVisible()
  expect(screen.getAllByRole('listitem')).toHaveLength(1)
})

it('手工添加和逐项移除目录', async () => {
  render(<LocalTasksPage api={api()} />)
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('手工目录'), 'D:\\Media')
  await user.click(screen.getByRole('button', { name: '添加目录' }))
  expect(screen.getByText('D:\\Media')).toBeVisible()
  await user.click(screen.getByRole('button', { name: '移除 D:\\Media' }))
  expect(screen.queryByText('D:\\Media')).not.toBeInTheDocument()
})

it('父目录替换子目录前使用可注入确认函数', async () => {
  const confirmReplace = vi.fn(async () => false)
  const choose = vi.fn().mockResolvedValueOnce(path('D:\\Media\\Photos')).mockResolvedValueOnce(path('D:\\Media'))
  render(<LocalTasksPage api={api({ choose })} confirmReplace={confirmReplace} />)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '选择目录…' }))
  await user.click(screen.getByRole('button', { name: '选择目录…' }))
  await waitFor(() => expect(confirmReplace).toHaveBeenCalledWith(['D:\\Media\\Photos'], 'D:\\Media'))
  expect(screen.getByText('D:\\Media\\Photos')).toBeVisible()
  expect(screen.queryByText('D:\\Media')).not.toBeInTheDocument()
})

it('创建扫描或自动三筛任务且一筛特征固定启用', async () => {
  const create = vi.fn(async () => ({ ok: true, task: { taskId: 't1' } }))
  render(<LocalTasksPage api={api({ create })} />)
  const user = userEvent.setup()
  expect(screen.getByText(/一筛基础特征.*默认计算/)).toBeVisible()
  expect(screen.queryByRole('checkbox', { name: /一筛/ })).not.toBeInTheDocument()
  await user.type(screen.getByLabelText('手工目录'), 'D:\\media')
  await user.click(screen.getByRole('button', { name: '添加目录' }))
  await user.selectOptions(screen.getByLabelText('任务模式'), 'scan_then_analysis')
  await user.click(screen.getByRole('button', { name: '创建任务' }))
  expect(create).toHaveBeenCalledWith(expect.objectContaining({ roots: ['D:\\media'], mode: 'scan_then_analysis' }))
})
