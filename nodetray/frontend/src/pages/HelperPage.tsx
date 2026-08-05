import { useEffect, useState, type ReactNode } from 'react'
import {
  ForceStopHelper,
  GetHelperForm,
  GetOverview,
  RestartHelper,
  SaveHelper,
  StartHelper,
  StopHelper,
  ValidateHelper,
} from '../../wailsjs/go/main/Backend'
import { config } from '../../wailsjs/go/models'
import { ActionBar } from '../components/ActionBar'
import { FormField } from '../components/FormField'
import { RootListField } from '../components/RootListField'
import { UACProgress, type UACPhase } from '../components/UACProgress'
import { useDirtyForm } from '../state/useDirtyForm'
import { useOptionalNodeState } from '../state/NodeStateContext'
import { lifecycleActions } from './OverviewPage'

type OperationResult = { ok: boolean; errorCode: string; errorSummary: string; uacCancelled: boolean }
type ConfigApplyResult = {
  ok: boolean; saved: boolean; restarted: boolean; sha256: string; needsRestart: boolean
  errorCode: string; errorSummary: string
}
type ComponentState = { lifecycle: string; needsAttention: boolean }
type HelperOverview = {
  helper: ComponentState
  helperStartMode: string
  helperEnabled: boolean
  helperTaskDrift: boolean
}

export type HelperPageDependencies = {
  getHelperForm: () => Promise<config.HelperForm>
  getOverview: () => Promise<HelperOverview>
  validateHelper: (form: config.HelperForm) => Promise<config.FieldError[]>
  saveHelper: (form: config.HelperForm) => Promise<ConfigApplyResult>
  startHelper: () => Promise<OperationResult>
  stopHelper: () => Promise<OperationResult>
  restartHelper: () => Promise<OperationResult>
  forceStopHelper: () => Promise<OperationResult>
  confirmHardDelete: () => Promise<boolean>
  confirmForceStop: () => Promise<boolean>
  chooseAllowedRoot?: () => Promise<string>
  chooseDeniedRoot?: () => Promise<string>
}

type HelperPageProps = {
  dependencies?: HelperPageDependencies
  onDirtyChange?: (dirty: boolean) => void
  onRequestExit?: () => void
}

const productionDependencies: HelperPageDependencies = {
  getHelperForm: GetHelperForm,
  getOverview: GetOverview,
  validateHelper: ValidateHelper,
  saveHelper: SaveHelper,
  startHelper: StartHelper,
  stopHelper: StopHelper,
  restartHelper: RestartHelper,
  forceStopHelper: ForceStopHelper,
  confirmHardDelete: async () => window.confirm('已启用硬删除。确认仅授权本次保存吗？'),
  confirmForceStop: async () => window.confirm('确认强制结束后端已认领并复核的 Helper 进程吗？'),
}

const emptyForm = new config.HelperForm({
  pipeName: '',
  allowedRoots: [],
  deniedRoots: [],
  defaultMode: 'soft',
  allowHardDelete: false,
  recycleDirName: '.recycle',
  maxEntriesPerFrame: 1,
  frameReadTimeoutSec: 1,
  frameWriteTimeoutSec: 1,
  logDir: '',
})

export function HelperPage({ dependencies = productionDependencies, onDirtyChange, onRequestExit }: HelperPageProps): ReactNode {
  const nodeState = useOptionalNodeState()
  const form = useDirtyForm(emptyForm)
  const commit = form.commit

  useEffect(() => {
    onDirtyChange?.(form.dirty)
  }, [form.dirty, onDirtyChange])
  const [overview, setOverview] = useState<HelperOverview | null>(null)
  const [loaded, setLoaded] = useState(false)
  const [pending, setPending] = useState(false)
  const [phase, setPhase] = useState<UACPhase>('idle')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [attention, setAttention] = useState('')
  const [status, setStatus] = useState('')
  const [stopTimedOut, setStopTimedOut] = useState(false)
  const currentOverview = nodeState?.snapshot.overview ?? overview

  useEffect(() => {
    let active = true
    Promise.all([dependencies.getHelperForm(), dependencies.getOverview()])
      .then(([loadedForm, loadedOverview]) => {
        if (!active) return
        commit(new config.HelperForm(loadedForm))
        setOverview(loadedOverview)
        setLoaded(true)
      })
      .catch(() => {
        if (active) setAttention('无法读取 Helper 配置。')
      })
    return () => { active = false }
  }, [dependencies, commit])

  const update = <K extends keyof config.HelperForm>(field: K, value: config.HelperForm[K]): void => {
    form.update((current) => new config.HelperForm({ ...current, [field]: value }))
    setStatus('')
  }

  const validate = async (): Promise<boolean> => {
    try {
      const fieldErrors = await dependencies.validateHelper(new config.HelperForm(form.value))
      setErrors(Object.fromEntries(fieldErrors.map((error) => [error.field, safeFieldMessage(error.message)])))
      if (fieldErrors.length > 0) {
        setAttention('配置校验未通过，请检查标记字段。')
        return false
      }
      setAttention('')
      return true
    } catch {
      setAttention('无法完成 Helper 配置校验。')
      return false
    }
  }

  const save = async (): Promise<void> => {
    if (pending || !(await validate())) return
    if (form.value.allowHardDelete && !(await dependencies.confirmHardDelete())) return
    setPending(true)
    setPhase('waiting')
    setStatus('')
    try {
      const result = await dependencies.saveHelper(new config.HelperForm(form.value))
      if (result.saved) {
        form.commit(new config.HelperForm(form.value))
        setStatus(configApplyStatus(result))
        await nodeState?.refresh()
      }
      if (!result.ok) {
        setPhase('failed')
        setAttention(operationFailure(result.errorCode))
        return
      }
      setPhase('succeeded')
      setAttention('')
    } catch {
      setPhase('failed')
      setAttention('Helper 配置保存失败。')
    } finally {
      setPending(false)
    }
  }

  const finishOperation = (result: OperationResult, success: string, onSuccess?: () => void): void => {
    if (result.uacCancelled) {
      setPhase('cancelled')
      setAttention('')
      setStatus('已取消。')
      return
    }
    if (!result.ok) {
      setPhase('failed')
      setStatus('')
      setAttention(operationFailure(result.errorCode))
      return
    }
    onSuccess?.()
    setPhase('succeeded')
    setAttention('')
    setStatus(success)
  }

  const lifecycle = async (
    action: () => Promise<OperationResult>,
    success: string,
    allowStopTimeout = false,
  ): Promise<void> => {
    if (pending || !currentOverview?.helperEnabled) return
    setPending(true)
    setPhase(currentOverview.helperStartMode === 'manual' ? 'waiting' : 'running')
    setAttention('')
    setStatus('')
    try {
      const result = await action()
      if (allowStopTimeout && !result.ok && result.errorCode === 'stop_timeout') {
        setPhase('failed')
        setStopTimedOut(true)
        return
      }
      if (!result.ok && result.errorCode === 'operation_conflict') void nodeState?.refresh()
      finishOperation(result, success)
    } catch {
      setPhase('failed')
      setAttention('Helper 操作失败。')
    } finally {
      setPending(false)
    }
  }

  const requestExitAll = (): void => {
    setStopTimedOut(false)
    onRequestExit?.()
  }

  const requestForceStop = async (): Promise<void> => {
    if (pending || !(await dependencies.confirmForceStop())) return
    setStopTimedOut(false)
    setPending(true)
    try {
      finishOperation(await dependencies.forceStopHelper(), 'Helper 强制结束请求已提交。')
    } finally {
      setPending(false)
    }
  }

  if (!loaded || !overview) {
    return <section aria-label="删除 Helper 配置"><h2>删除 Helper 配置</h2>{attention ? <p role="alert">{attention}</p> : <p role="status">正在读取配置…</p>}</section>
  }

  const textField = (field: keyof config.HelperForm, label: string, value: string): ReactNode => (
    <FormField id={`helper-${field}`} label={label} error={errors[field]}>
      <input name={field} type="text" value={value} disabled={pending} onChange={(event) => update(field, event.currentTarget.value)} />
    </FormField>
  )
  const numberField = (field: keyof config.HelperForm, label: string, value: number): ReactNode => (
    <FormField id={`helper-${field}`} label={label} error={errors[field]}>
      <input name={field} type="number" min={1} step={1} value={value} disabled={pending} onChange={(event) => update(field, Number(event.currentTarget.value))} />
    </FormField>
  )
  const helperOverview = currentOverview ?? overview
  const helperState = helperOverview.helper
  const availableActions = lifecycleActions(helperState.lifecycle)

  return (
    <section aria-label="删除 Helper 配置">
      <h2>删除 Helper 配置</h2>
      <dl>
        <div><dt>组件</dt><dd>{helperOverview.helperEnabled ? '已启用' : '已禁用'}</dd></div>
        <div><dt>启动方式</dt><dd>{helperOverview.helperStartMode === 'automatic' ? '自动（固定计划任务）' : '手动'}</dd></div>
        <div><dt>生命周期</dt><dd>{helperState.lifecycle}</dd></div>
      </dl>
      {!helperOverview.helperEnabled ? <p>Helper 已禁用；仍可编辑并保存配置。</p> : null}
      {helperOverview.helperStartMode === 'manual' ? <p>启动或重启 Helper 将请求管理员权限。</p> : <p>自动模式使用应用固定的计划任务配置。</p>}
      {helperOverview.helperStartMode === 'automatic' && helperOverview.helperTaskDrift ? <p role="alert">固定计划任务配置已漂移。</p> : null}

      <ActionBar ariaLabel="Helper 生命周期操作">
        <button type="button" className="button-primary" disabled={pending || !helperOverview.helperEnabled || !availableActions.start} onClick={() => void lifecycle(dependencies.startHelper, 'Helper 启动请求已提交。')}>启动 Helper</button>
        <button type="button" className="button-secondary" disabled={pending || !helperOverview.helperEnabled || !availableActions.stop} onClick={() => void lifecycle(dependencies.stopHelper, 'Helper 停止请求已提交。', true)}>{availableActions.cancelStart ? '取消启动' : '停止 Helper'}</button>
        <button type="button" className="button-secondary" disabled={pending || !helperOverview.helperEnabled || !availableActions.restart} onClick={() => void lifecycle(dependencies.restartHelper, 'Helper 重启请求已提交。')}>重启 Helper</button>
      </ActionBar>
      <UACProgress phase={phase} />
      {attention ? <p role="alert">{attention}</p> : null}
      {status ? <p role="status">{status}</p> : null}

      <form onSubmit={(event) => event.preventDefault()}>
        <fieldset disabled={pending}><legend>管道</legend>{textField('pipeName', '管道名称', form.value.pipeName)}</fieldset>
        <fieldset disabled={pending}><legend>路径</legend>
          <RootListField id="allowed-roots" name="allowedRoots" label="允许的媒体根目录" values={form.value.allowedRoots} errors={errors} disabled={pending} chooseRoot={dependencies.chooseAllowedRoot} onChange={(values) => update('allowedRoots', values)} />
          <RootListField id="denied-roots" name="deniedRoots" label="拒绝的媒体根目录" values={form.value.deniedRoots} errors={errors} disabled={pending} chooseRoot={dependencies.chooseDeniedRoot} onChange={(values) => update('deniedRoots', values)} />
          {textField('recycleDirName', '回收目录名称', form.value.recycleDirName)}
        </fieldset>
        <fieldset disabled={pending}><legend>删除</legend>
          <FormField id="helper-defaultMode" label="默认删除模式" error={errors.defaultMode}>
            <select name="defaultMode" value={form.value.defaultMode} onChange={(event) => update('defaultMode', event.currentTarget.value)}>
              <option value="soft">软删除</option>
              <option value="hard">硬删除</option>
            </select>
          </FormField>
          <FormField id="helper-allowHardDelete" label="允许硬删除" error={errors.allowHardDelete}>
            <input name="allowHardDelete" type="checkbox" checked={form.value.allowHardDelete} onChange={(event) => update('allowHardDelete', event.currentTarget.checked)} />
          </FormField>
          {form.value.allowHardDelete ? <p role="alert">高风险：硬删除不可通过回收目录恢复，每次保存都需要再次确认。</p> : null}
        </fieldset>
        <fieldset disabled={pending}><legend>协议</legend>
          {numberField('maxEntriesPerFrame', '每帧最大条目', form.value.maxEntriesPerFrame)}
          {numberField('frameReadTimeoutSec', '读取超时', form.value.frameReadTimeoutSec)}
          {numberField('frameWriteTimeoutSec', '写入超时', form.value.frameWriteTimeoutSec)}
        </fieldset>
        <fieldset disabled={pending}><legend>日志</legend>{textField('logDir', '日志目录', form.value.logDir)}</fieldset>
        <ActionBar ariaLabel="Helper 配置操作">
          <button type="button" className="button-primary" disabled={pending} onClick={() => void save()}>保存 Helper 配置</button>
          <button type="button" className="button-secondary" disabled={pending || !form.dirty} onClick={() => form.reset()}>撤销未保存更改</button>
        </ActionBar>
      </form>

      {stopTimedOut ? (
        <div className="dialog-backdrop">
          <div className="confirm-dialog" role="dialog" aria-modal="true" aria-labelledby="helper-stop-timeout-title">
            <h2 id="helper-stop-timeout-title">Helper 停止超时</h2>
            <p>请选择返回、打开统一强制退出确认，或仅强制结束 Helper。</p>
            <ActionBar ariaLabel="停止超时操作">
              <button type="button" className="button-secondary" onClick={() => setStopTimedOut(false)}>返回</button>
              <button type="button" className="button-secondary" onClick={requestExitAll}>强制退出全部</button>
              <button type="button" className="button-primary" onClick={() => void requestForceStop()}>强制结束已认领 Helper</button>
            </ActionBar>
          </div>
        </div>
      ) : null}
    </section>
  )
}

function safeFieldMessage(message: string): string {
  return String(message || '').replace(/[A-Za-z]:[\\/][^\s,;]*/g, '[路径已隐藏]').trim().slice(0, 160) || '字段值无效。'
}

function operationFailure(code: string): string {
  const safeCode = code.replace(/[^a-z0-9_-]/gi, '').slice(0, 48)
  return safeCode ? `操作失败（${safeCode}）。` : '操作失败，请稍后重试。'
}

function configApplyStatus(result: ConfigApplyResult): string {
  const digest = result.sha256 ? `（${result.sha256.slice(0, 12)}）` : ''
  if (result.needsRestart && !result.restarted) return `配置已保存，需要重启后生效${digest}。`
  return `Helper 配置已保存${digest}。`
}
