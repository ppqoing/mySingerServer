import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { DeleteHistoryPage } from './DeleteHistoryPage'

it('先预览再确认且失败或不确定项不会显示为已删除', async () => {
  const execute = vi.fn(async () => ({ ok: true, succeeded: 1, failed: 1, uncertain: 1, items: [
    { fileId: 1, result: 'deleted', uncertain: false }, { fileId: 2, result: 'failed', uncertain: false }, { fileId: 3, result: 'deleted', uncertain: true },
  ] }))
  render(<DeleteHistoryPage api={{ prepare: vi.fn(async () => ({ ok: true, batchId: 'b', selectionDigest: 'd', count: 3, totalSize: 99, files: [] })), execute }} />)
  await userEvent.type(screen.getByLabelText('运行 ID'), 'r')
  await userEvent.type(screen.getByLabelText('分组 ID'), 'g')
  await userEvent.click(screen.getByRole('button', { name: '预览删除' }))
  expect(await screen.findByText(/共 3 个文件/)).toBeVisible()
  await userEvent.click(screen.getByRole('button', { name: '确认删除' }))
  expect(await screen.findByText('已删除 1')).toBeVisible()
  expect(screen.getByText('失败 1')).toBeVisible()
  expect(screen.getByText('不确定 1')).toBeVisible()
})
