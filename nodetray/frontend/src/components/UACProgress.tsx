import type { ReactNode } from 'react'

export type UACPhase = 'idle' | 'waiting' | 'running' | 'cancelled' | 'succeeded' | 'failed'

const phaseText: Record<Exclude<UACPhase, 'idle'>, string> = {
  waiting: '等待系统确认',
  running: '正在执行需要管理员权限的操作',
  cancelled: '已取消',
  succeeded: '操作已完成',
  failed: '操作失败',
}

export function UACProgress({ phase }: { phase: UACPhase }): ReactNode {
  if (phase === 'idle') return null
  return <p className="uac-progress" role="status" aria-live="polite">{phaseText[phase]}</p>
}
