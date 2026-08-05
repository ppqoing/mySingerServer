import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { ActionBar } from './ActionBar'
import { ComponentCard } from './ComponentCard'
import { ConfirmDialog } from './ConfirmDialog'
import { FormField } from './FormField'
import { StatusBadge } from './StatusBadge'

describe('基础组件', () => {
  it('ComponentCard 在 pending 时禁用操作区内的冲突动作', () => {
    render(
      <ComponentCard
        title="Agent"
        status={<StatusBadge lifecycle="starting" />}
        summary="正在等待组件启动。"
        pending
        actions={<button type="button">停止 Agent</button>}
      />,
    )

    expect(screen.getByRole('button', { name: '停止 Agent' })).toBeDisabled()
    expect(screen.getByRole('article', { name: 'Agent' })).toHaveAttribute('aria-busy', 'true')
  })

  it('FormField 一致关联 label、help、error 和控件', () => {
    render(
      <FormField id="pipe-name" label="管道名称" help="使用节点内唯一名称。" error="管道名称不能为空。">
        <input />
      </FormField>,
    )

    const input = screen.getByRole('textbox', { name: '管道名称' })
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(input).toHaveAccessibleDescription('使用节点内唯一名称。 管道名称不能为空。')
    expect(screen.getByRole('alert')).toHaveTextContent('管道名称不能为空。')
  })

  it('ActionBar 提供有名称的操作组', () => {
    render(
      <ActionBar ariaLabel="配置操作">
        <button type="button">测试配置</button>
        <button type="button">保存</button>
      </ActionBar>,
    )

    expect(screen.getByRole('group', { name: '配置操作' })).toContainElement(
      screen.getByRole('button', { name: '保存' }),
    )
  })
})

function DialogHarness({ closeOnEscape = true }: { closeOnEscape?: boolean }) {
  const [open, setOpen] = useState(false)

  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        删除配置
      </button>
      <ConfirmDialog
        open={open}
        title="确认删除配置"
        description="只删除本机保存的测试配置。"
        confirmLabel="确认删除"
        closeOnEscape={closeOnEscape}
        onConfirm={() => setOpen(false)}
        onCancel={() => setOpen(false)}
      />
    </>
  )
}

describe('ConfirmDialog', () => {
  it('打开后聚焦安全动作，并让 Tab 与 Shift+Tab 在对话框内循环', async () => {
    const user = userEvent.setup()
    render(<DialogHarness />)

    await user.click(screen.getByRole('button', { name: '删除配置' }))
    const cancel = screen.getByRole('button', { name: '取消' })
    const confirm = screen.getByRole('button', { name: '确认删除' })
    expect(cancel).toHaveFocus()

    await user.keyboard('{Shift>}{Tab}{/Shift}')
    expect(confirm).toHaveFocus()
    await user.keyboard('{Tab}')
    expect(cancel).toHaveFocus()
  })

  it('允许 Escape 关闭时关闭并把焦点返回触发器', async () => {
    const user = userEvent.setup()
    render(<DialogHarness />)

    const trigger = screen.getByRole('button', { name: '删除配置' })
    await user.click(trigger)
    await user.keyboard('{Escape}')

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
  })

  it('策略禁止 Escape 关闭时保持对话框打开', async () => {
    const user = userEvent.setup()
    render(<DialogHarness closeOnEscape={false} />)

    await user.click(screen.getByRole('button', { name: '删除配置' }))
    await user.keyboard('{Escape}')

    expect(screen.getByRole('dialog', { name: '确认删除配置' })).toBeVisible()
  })
})
