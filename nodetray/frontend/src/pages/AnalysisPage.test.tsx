import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import type { LocalTask } from '../api/localAgent'
import { AnalysisPage } from './AnalysisPage'

const task: LocalTask = {
  taskId: 't1',
  instanceId: 'analysis-instance-1',
  revision: 1,
  source: 'nodetray',
  mode: 'scan_then_analysis',
  stage: 2,
  status: 'running',
  phase: 'stage2',
  roots: ['D:\\Media'],
  progressComplete: 12,
  progressTotal: 100,
  progressTotalKnown: true,
  speed: '8/s',
  failures: 1,
  duration: '12s',
  syncStatus: '同步暂不可用',
  createdAt: 1,
  updatedAt: 2,
  startedAt: 1,
  completedAt: 0,
}

it('展示本机任务阶段指标且PG失败只降级同步状态', async () => {
  render(<AnalysisPage api={{ list: vi.fn(async () => ({ ok: true, tasks: [task] })) }} />)
  expect(await screen.findByText('nodetray')).toBeVisible()
  for (const text of ['阶段', '状态', '速度', '失败数', '耗时', '同步暂不可用']) expect(screen.getByText(text)).toBeVisible()
  expect(screen.getByText('running')).toBeVisible()
})
