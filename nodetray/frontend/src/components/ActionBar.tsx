import type { ReactNode } from 'react'

type ActionBarProps = {
  ariaLabel: string
  children: ReactNode
}

export function ActionBar({ ariaLabel, children }: ActionBarProps): ReactNode {
  return (
    <div className="action-bar" role="group" aria-label={ariaLabel}>
      {children}
    </div>
  )
}
