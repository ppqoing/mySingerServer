import { useEffect, useRef, type KeyboardEvent, type ReactNode } from 'react'
import { ActionBar } from './ActionBar'

type ConfirmDialogProps = {
  open: boolean
  title: string
  description: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  closeOnEscape?: boolean
  onConfirm: () => void
  onCancel: () => void
}

export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel = '确认',
  cancelLabel = '取消',
  closeOnEscape = true,
  onConfirm,
  onCancel,
}: ConfirmDialogProps): ReactNode {
  const dialogRef = useRef<HTMLDivElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const returnFocusRef = useRef<HTMLElement | null>(null)
  const wasOpenRef = useRef(false)
  const titleID = 'confirm-dialog-title'
  const descriptionID = 'confirm-dialog-description'

  useEffect(() => {
    if (open && !wasOpenRef.current) {
      returnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
      cancelRef.current?.focus()
    }
    if (!open && wasOpenRef.current) {
      returnFocusRef.current?.focus()
      returnFocusRef.current = null
    }
    wasOpenRef.current = open
  }, [open])

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (event.key === 'Escape') {
      event.preventDefault()
      if (closeOnEscape) {
        onCancel()
      }
      return
    }
    if (event.key !== 'Tab') {
      return
    }

    const focusable = Array.from(
      dialogRef.current?.querySelectorAll<HTMLElement>('button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])') ?? [],
    )
    if (focusable.length === 0) {
      event.preventDefault()
      return
    }
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first.focus()
    }
  }

  if (!open) {
    return null
  }

  return (
    <div className="dialog-backdrop">
      <div
        className="confirm-dialog"
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        aria-describedby={descriptionID}
        onKeyDown={onKeyDown}
      >
        <h2 id={titleID}>{title}</h2>
        <p id={descriptionID}>{description}</p>
        <ActionBar ariaLabel="确认操作">
          <button className="button-secondary" ref={cancelRef} type="button" onClick={onCancel}>
            {cancelLabel}
          </button>
          <button className="button-primary" type="button" onClick={onConfirm}>
            {confirmLabel}
          </button>
        </ActionBar>
      </div>
    </div>
  )
}
