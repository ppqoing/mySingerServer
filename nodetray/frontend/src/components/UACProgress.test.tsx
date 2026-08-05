import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import { UACProgress } from './UACProgress'

describe('UACProgress', () => {
  it.each([
    ['waiting', '等待系统确认'],
    ['running', '正在执行需要管理员权限的操作'],
    ['cancelled', '已取消'],
    ['succeeded', '操作已完成'],
    ['failed', '操作失败'],
  ] as const)('阶段 %s 只显示稳定可访问状态', (phase, text) => {
    render(<UACProgress phase={phase} />)
    const status = screen.getByRole('status')
    expect(status).toHaveTextContent(text)
    expect(status).not.toHaveTextContent(/nonce|cmd|account|password|C:\\/i)
  })

  it('空闲阶段不占用页面状态区', () => {
    render(<UACProgress phase="idle" />)
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })
})
