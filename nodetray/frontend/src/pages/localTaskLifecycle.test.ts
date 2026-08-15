import {
  actionsForTaskStatus,
  isActiveLocalTaskStatus,
  phaseLabelForTask,
  statusLabelForTask,
  type LocalTaskOperation,
  type LocalTaskStatus,
} from './localTaskLifecycle'

const statusCases: Array<readonly [LocalTaskStatus, string, readonly LocalTaskOperation[], boolean]> = [
  ['pending', '等待中', ['pause', 'cancel', 'delete'], true],
  ['running', '运行中', ['pause', 'cancel', 'delete'], true],
  ['waiting_recovery', '等待恢复', ['pause', 'cancel', 'delete'], true],
  ['pausing', '正在暂停', [], true],
  ['paused', '已暂停', ['resume', 'cancel', 'delete'], false],
  ['stopping', '正在停止', [], true],
  ['cancelled', '已停止', ['retry', 'delete'], false],
  ['succeeded', '已完成', ['delete'], false],
  ['failed', '失败', ['retry', 'delete'], false],
  ['deleting', '正在删除', [], true],
  ['delete_failed', '删除失败', ['delete'], false],
]

describe('本地任务生命周期', () => {
  it.each(statusCases)('为 %s 提供精确的中文文案、活动态和操作', (status, label, actions, active) => {
    expect(statusLabelForTask(status)).toBe(label)
    expect(actionsForTaskStatus(status)).toEqual(actions)
    expect(isActiveLocalTaskStatus(status)).toBe(active)
  })

  it.each([
    ['waiting', '等待'],
    ['scan', '枚举与扫描'],
    ['stage1', '一筛'],
    ['stage2', '二筛'],
    ['stage3', '三筛'],
    ['finalizing', '安全收尾'],
  ])('为阶段 %s 提供中文文案', (phase, label) => {
    expect(phaseLabelForTask(phase)).toBe(label)
  })

  it('将未来未知状态和阶段安全降级且不提供操作', () => {
    expect(statusLabelForTask('future_status')).toBe('未知状态')
    expect(phaseLabelForTask('future_phase')).toBe('未知阶段')
    expect(actionsForTaskStatus('future_status')).toEqual([])
    expect(isActiveLocalTaskStatus('future_status')).toBe(false)
  })
})
