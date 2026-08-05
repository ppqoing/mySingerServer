import { useEffect, useRef, useState, type ReactNode } from 'react'
import { ForceExitAll } from '../../wailsjs/go/main/Backend'
import { ActionBar } from './ActionBar'

type ForceExitResult = {
  ok: boolean
  failedComponents: string[]
  errorCode: string
  errorSummary: string
}

export type ExitDialogDependencies = {
  forceExitAll: () => Promise<ForceExitResult>
}

type ExitDialogProps = {
  open: boolean
  dirty: boolean
  dependencies?: ExitDialogDependencies
  onReturn: () => void
}

const productionDependencies: ExitDialogDependencies = { forceExitAll: ForceExitAll }

export function ExitDialog({ open, dirty, dependencies = productionDependencies, onReturn }: ExitDialogProps): ReactNode {
  const [pending, setPending] = useState(false)
  const [failedComponents, setFailedComponents] = useState<string[]>([])
  const [attention, setAttention] = useState('')
  const cancelRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    if (!open) return
    cancelRef.current?.focus()
  }, [open])

  if (!open) return null

  const forceExit = async (): Promise<void> => {
    if (pending) return
    setPending(true)
    setAttention('')
    try {
      const result = await dependencies.forceExitAll()
      if (!result.ok) {
        setFailedComponents(result.failedComponents ?? [])
        setAttention('部分后台进程仍未退出，界面将保持打开。')
      }
    } catch {
      setFailedComponents([])
      setAttention('强制退出请求失败，界面将保持打开。')
    } finally {
      setPending(false)
    }
  }

  const retrying = attention !== ''
  const returnToApp = (): void => {
    setFailedComponents([])
    setAttention('')
    onReturn()
  }
  return (
    <div className="dialog-backdrop">
      <div className="confirm-dialog" role="dialog" aria-modal="true" aria-labelledby="exit-dialog-title">
        <h2 id="exit-dialog-title">确认强制退出</h2>
        <p>确认后将强制结束全部已记录后台进程；后台全部退出后再关闭界面。</p>
        {dirty ? <p>存在未保存修改；继续退出时未保存更改将丢失。</p> : null}
        {attention ? (
          <p role="alert">
            {attention}
            {failedComponents.length > 0 ? ` 失败组件：${failedComponents.join('、')}` : ''}
          </p>
        ) : null}
        <ActionBar ariaLabel="强制退出确认">
          <button ref={cancelRef} type="button" className="button-secondary" disabled={pending} onClick={returnToApp}>取消</button>
          <button type="button" className="button-primary" disabled={pending} onClick={() => void forceExit()}>
            {retrying ? '重试强制退出' : '强制退出全部后台进程'}
          </button>
        </ActionBar>
      </div>
    </div>
  )
}
