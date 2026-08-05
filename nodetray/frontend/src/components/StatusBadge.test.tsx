import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import { StatusBadge } from './StatusBadge'

describe('StatusBadge', () => {
  it.each([
    ['stopped', '已停止'],
    ['starting', '启动中'],
    ['running', '运行中'],
    ['stopping', '停止中'],
    ['failed', '异常'],
  ])('生命周期 %s 同时显示本地图标和中文文本', (lifecycle, label) => {
    const { container } = render(<StatusBadge lifecycle={lifecycle} />)

    expect(screen.getByText(label)).toBeVisible()
    expect(container.querySelector('svg')).toHaveAttribute('aria-hidden', 'true')
  })

  it('未知生命周期使用稳定的安全降级文本', () => {
    render(<StatusBadge lifecycle="future-state" />)

    expect(screen.getByText('状态未知')).toBeVisible()
  })

  it('disabled 覆盖错误生命周期并使用中性未启用展示', () => {
    const { container } = render(<StatusBadge lifecycle="failed" disabled />)

    expect(screen.getByText('未启用')).toBeVisible()
    expect(container.querySelector('.status-badge')).toHaveAttribute('data-lifecycle', 'disabled')
    expect(container.querySelector('svg')).toHaveAttribute('data-icon', 'pause')
    expect(screen.queryByText('异常')).not.toBeInTheDocument()
  })
})
