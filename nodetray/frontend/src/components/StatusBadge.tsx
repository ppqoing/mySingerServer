import type { ReactNode } from 'react'

type StatusDefinition = {
  label: string
  icon: 'pause' | 'spinner' | 'check' | 'stop' | 'warning' | 'unknown'
}

const definitions: Readonly<Record<string, StatusDefinition>> = {
  stopped: { label: '已停止', icon: 'pause' },
  starting: { label: '启动中', icon: 'spinner' },
  running: { label: '运行中', icon: 'check' },
  stopping: { label: '停止中', icon: 'stop' },
  failed: { label: '异常', icon: 'warning' },
}

const fallback: StatusDefinition = { label: '状态未知', icon: 'unknown' }
const disabledDefinition: StatusDefinition = { label: '未启用', icon: 'pause' }

function StatusIcon({ kind }: { kind: StatusDefinition['icon'] }): ReactNode {
  return (
    <svg
      className="status-badge__icon"
      viewBox="0 0 16 16"
      aria-hidden="true"
      focusable="false"
      data-icon={kind}
    >
      <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="2" />
      {kind === 'check' ? <path d="m5 8 2 2 4-5" fill="none" stroke="currentColor" strokeWidth="2" /> : null}
      {kind === 'pause' ? <path d="M6 5v6M10 5v6" stroke="currentColor" strokeWidth="2" /> : null}
      {kind === 'spinner' ? <path d="M8 2a6 6 0 0 1 6 6" fill="none" stroke="currentColor" strokeWidth="2" /> : null}
      {kind === 'stop' ? <rect x="5" y="5" width="6" height="6" fill="currentColor" /> : null}
      {kind === 'warning' ? <path d="M8 4v5m0 2v1" stroke="currentColor" strokeWidth="2" /> : null}
      {kind === 'unknown' ? <path d="M6.5 6a1.5 1.5 0 1 1 2.2 1.3C8 7.7 8 8.1 8 9m0 2v1" fill="none" stroke="currentColor" /> : null}
    </svg>
  )
}

export function StatusBadge({
  lifecycle,
  disabled = false,
}: {
  lifecycle: string
  disabled?: boolean
}): ReactNode {
  const definition = disabled ? disabledDefinition : definitions[lifecycle] ?? fallback
  const safeLifecycle = disabled ? 'disabled' : lifecycle in definitions ? lifecycle : 'unknown'

  return (
    <span className="status-badge" data-lifecycle={safeLifecycle}>
      <StatusIcon kind={definition.icon} />
      <span>{definition.label}</span>
    </span>
  )
}
