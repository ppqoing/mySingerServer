import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import { AnalysisPage } from './AnalysisPage'

it('展示本机任务阶段指标且PG失败只降级同步状态', async () => {
  render(<AnalysisPage api={{ list: vi.fn(async () => ({ ok: true, tasks: [{ taskId: 't1', source: 'nodetray', stage: 2, status: 'running', speed: '8/s', failures: 1, duration: '12s', syncStatus: '同步暂不可用' }] })) }} />)
  expect(await screen.findByText('nodetray')).toBeVisible()
  for (const text of ['阶段', '状态', '速度', '失败数', '耗时', '同步暂不可用']) expect(screen.getByText(text)).toBeVisible()
  expect(screen.getByText('running')).toBeVisible()
})
