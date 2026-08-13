import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ReviewPage } from './ReviewPage'

it('按四类展示三筛结果，删除默认未选，预览仅请求文件ID', async () => {
  const preview = vi.fn(async () => ({ ok: true, mime: 'image/jpeg', dataBase64: 'AA==', width: 20, height: 10 }))
  const save = vi.fn(async () => ({ ok: true }))
  render(<ReviewPage api={{ list: vi.fn(async () => ({ ok: true, groups: [{ runId: 'r', groupId: 'g', category: 'image', verdict: 'duplicate', stageOne: '相同', stageTwo: '0.91', stageThree: '0.96', members: [{ fileId: 7, fileName: 'a.jpg', size: 12, decision: 'undecided' }] }] })), save, preview }} />)
  for (const text of ['精确重复', '图片相似', '视频相似', '待确认', '一筛', '二筛', '三筛']) expect(await screen.findByText(text)).toBeVisible()
  expect(screen.getByLabelText('删除 a.jpg')).not.toBeChecked()
  await userEvent.click(screen.getByRole('button', { name: '预览 a.jpg' }))
  expect(preview).toHaveBeenCalledWith(7)
  expect(await screen.findByRole('img', { name: 'a.jpg 预览' })).toHaveAttribute('src', 'data:image/jpeg;base64,AA==')
  expect(document.body.innerHTML).not.toContain('file://')
  await userEvent.click(screen.getByRole('button', { name: '保存审核' }))
  expect(save).toHaveBeenCalledWith(expect.objectContaining({ runId: 'r', groupId: 'g', decisions: [{ fileId: 7, decision: 'keep' }] }))
})
