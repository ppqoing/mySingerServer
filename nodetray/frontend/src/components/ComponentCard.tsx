import type { ReactNode } from 'react'

type ComponentCardProps = {
  title: string
  status: ReactNode
  summary: ReactNode
  actions?: ReactNode
  pending?: boolean
}

export function ComponentCard({ title, status, summary, actions, pending = false }: ComponentCardProps): ReactNode {
  return (
    <article className="component-card" aria-label={title} aria-busy={pending}>
      <div className="component-card__heading">
        <h3>{title}</h3>
        {status}
      </div>
      <div className="component-card__summary">{summary}</div>
      {actions ? (
        <fieldset className="component-card__actions" disabled={pending}>
          <legend className="visually-hidden">{title} 操作</legend>
          {actions}
        </fieldset>
      ) : null}
    </article>
  )
}
