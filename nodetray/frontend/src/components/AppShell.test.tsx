import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AppShell } from './AppShell'

describe('AppShell', () => {
  beforeEach(() => {
    window.location.hash = ''
  })

  it('包含本机闭环页签且不提供其他机器连接入口', () => {
    render(<AppShell />)

    expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual([
      '总览',
      'Agent',
      '删除 Helper',
      '程序设置',
	  '本地任务',
	  '去重分析',
	  '结果审核',
	  '删除记录',
    ])
	expect(screen.queryByRole('tab', { name: /其他机器|远程 Agent/ })).not.toBeInTheDocument()
    expect(screen.getByRole('tab', { name: '总览' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('status')).toHaveTextContent('当前页面：总览')

    const panels = screen.getAllByRole('tabpanel', { hidden: true })
    expect(panels).toHaveLength(8)
    for (const tab of screen.getAllByRole('tab')) {
      const panel = panels.find((candidate) => candidate.id === tab.getAttribute('aria-controls'))
      expect(panel).toHaveAttribute('aria-labelledby', tab.id)
    }
  })

  it('保持循环方向键、Home 和 End 的键盘页签行为', async () => {
    const user = userEvent.setup()
    render(<AppShell />)

    const overview = screen.getByRole('tab', { name: '总览' })
    overview.focus()
    await user.keyboard('{ArrowLeft}')
    expect(screen.getByRole('tab', { name: '删除记录' })).toHaveFocus()
    expect(screen.getByRole('status')).toHaveTextContent('当前页面：删除记录')

    await user.keyboard('{Home}')
    expect(overview).toHaveFocus()
    await user.keyboard('{End}')
    expect(screen.getByRole('tab', { name: '删除记录' })).toHaveFocus()
    await user.keyboard('{ArrowRight}')
    expect(overview).toHaveFocus()
  })

  it('未知本地 hash 安全回退到总览且壳层禁止横向溢出', () => {
    window.location.hash = '#/not-supported'
    render(<AppShell />)

    expect(screen.getByRole('tabpanel', { name: '总览' })).toBeVisible()
    expect(screen.getByRole('main')).toHaveStyle({ overflowX: 'hidden' })
  })

  it('可注入四个真实 panel 且不改变固定页签合同', async () => {
    const user = userEvent.setup()
    render(<AppShell panels={{
      overview: <p>真实总览</p>,
      agent: <p>真实 Agent</p>,
      helper: <p>真实 Helper</p>,
      settings: <p>真实设置</p>,
    }} />)
    expect(screen.getByText('真实总览')).toBeVisible()
    await user.click(screen.getByRole('tab', { name: 'Agent' }))
    expect(screen.getByText('真实 Agent')).toBeVisible()
    expect(screen.getAllByRole('tab')).toHaveLength(8)
  })
})
