import { useEffect, useState, type ReactNode } from 'react'
import {
  GetAgentForm,
  RestartAgent,
  SaveAgent,
  SaveAndRestartAgent,
  StartAgent,
  StopAgent,
  ValidateAgent,
} from '../../wailsjs/go/main/Backend'
import { config } from '../../wailsjs/go/models'
import { ActionBar } from '../components/ActionBar'
import { DatabaseFields } from '../components/DatabaseFields'
import { FormField } from '../components/FormField'
import { PathPicker } from '../components/PathPicker'
import { TagListField } from '../components/TagListField'
import { WorkerSummary } from '../components/WorkerSummary'
import { lifecycleActions } from './OverviewPage'
import { useOptionalNodeState } from '../state/NodeStateContext'
import type { ComponentState, WorkerState } from '../state/nodeStore'
import { useDirtyForm } from '../state/useDirtyForm'

type OperationResult = { ok: boolean; errorCode: string; errorSummary: string; uacCancelled: boolean }
type ConfigApplyResult = {
  ok: boolean; saved: boolean; restarted: boolean; sha256: string; needsRestart: boolean
  errorCode: string; errorSummary: string
}

export type AgentPageDependencies = {
  getAgentForm: () => Promise<config.AgentForm>
  validateAgent: (form: config.AgentForm) => Promise<config.FieldError[]>
  saveAgent: (form: config.AgentForm) => Promise<ConfigApplyResult>
  saveAndRestartAgent: (form: config.AgentForm) => Promise<ConfigApplyResult>
  startAgent: () => Promise<OperationResult>
  stopAgent: () => Promise<OperationResult>
  restartAgent: () => Promise<OperationResult>
  confirmRestart: () => Promise<boolean>
  copyText: (text: string) => Promise<void>
  choosePath?: (currentPath: string) => Promise<string>
}

type AgentPageProps = {
  dependencies?: AgentPageDependencies
  workers?: WorkerState[]
  workerReady?: number
  workerExpected?: number
  onDirtyChange?: (dirty: boolean) => void
  componentState?: Pick<ComponentState, 'lifecycle'>
}

const productionDependencies: AgentPageDependencies = {
  getAgentForm: GetAgentForm,
  validateAgent: ValidateAgent,
  saveAgent: SaveAgent,
  saveAndRestartAgent: SaveAndRestartAgent,
  startAgent: StartAgent,
  stopAgent: StopAgent,
  restartAgent: RestartAgent,
  confirmRestart: async () => window.confirm('保存配置并重启 Agent 会中断当前处理，是否继续？'),
  copyText: async (text) => navigator.clipboard.writeText(text),
}

const emptyForm: config.AgentForm = new config.AgentForm({
  listenHost: '', listenPort: 0, dataDir: '',
  database: { host: '', port: 5432, database: '', user: '', password: '', passwordStored: false, replacePassword: false, sslMode: 'prefer' },
  useEverything: false,
  scan: { hddReadBlockMb: 1, hddStreamsPerDisk: 1, ssdStreamsPerDisk: 1, imageMemResidentMb: 0, imageTimeoutS: 1, videoTimeoutS: 1, imageExts: [], videoExts: [] },
  sync: { intervalS: 1, triggerRows: 1, upsertBatch: 1 },
  proto: { heartbeatS: 1 },
  worker: { count: 1, exePath: '', imageTimeoutS: 1, videoTimeoutS: 1, imageMemoryMb: 0, respawnDelayMs: 0 },
  pipeline: { readChunkKb: 1 },
  thumb: { cacheDir: '', tileMaxSide: 1, probeTimeoutS: 1, nativeTimeoutS: 1, frameTimeoutS: 1 },
  ipc: { maxFrameMb: 1 },
  delete: { pipeName: '', maxEntriesPerFrame: 1, dialTimeoutMs: 1, helloTimeoutS: 1, reportTimeoutS: 1 },
  tuning: { statsEnabled: false, statsIntervalS: 1, statsHistoryS: 1, pendingBytesMb: 0, statsLogMb: 0, pprofAddr: '' },
})

export function AgentPage({
  dependencies = productionDependencies,
  workers = [],
  workerReady = 0,
  workerExpected = workers.length,
  onDirtyChange,
  componentState,
}: AgentPageProps): ReactNode {
  const nodeState = useOptionalNodeState()
  const lifecycle = componentState?.lifecycle ?? nodeState?.snapshot.overview?.agent.lifecycle ?? 'running'
  const availableActions = lifecycleActions(lifecycle)
  const form = useDirtyForm(emptyForm)
  const [loaded, setLoaded] = useState(false)
  const [pending, setPending] = useState(false)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [validationCodes, setValidationCodes] = useState<Record<string, string>>({})
  const [status, setStatus] = useState('')
  const [attention, setAttention] = useState('')
  const commitLoadedForm = form.commit

  useEffect(() => {
    onDirtyChange?.(form.dirty)
  }, [form.dirty, onDirtyChange])

  useEffect(() => {
    let active = true
    dependencies.getAgentForm().then((loadedForm) => {
      if (active) {
        commitLoadedForm(withPasswordHidden(loadedForm))
        setLoaded(true)
      }
    }).catch(() => {
      if (active) {
        setAttention('无法读取 Agent 配置。')
      }
    })
    return () => { active = false }
  }, [dependencies, commitLoadedForm])

  const updateField = (path: string, value: unknown): void => {
    form.update((current) => setAtPath(current, path, value))
    setStatus('')
  }

  const mapErrors = (fieldErrors: config.FieldError[]): Record<string, string> => Object.fromEntries(
    fieldErrors.map((error) => [error.field, safeMessage(error.message, form.value)]),
  )
  const mapCodes = (fieldErrors: config.FieldError[]): Record<string, string> => Object.fromEntries(
    fieldErrors.map((error) => [error.field, error.code]),
  )

  const validateField = async (field: string): Promise<void> => {
    try {
      const fieldErrors = await dependencies.validateAgent(submissionForm(form.value))
      const match = fieldErrors.find((error) => error.field === field)
      setErrors((current) => {
        const next = { ...current }
        if (match) next[field] = safeMessage(match.message, form.value)
        else delete next[field]
        return next
      })
      setValidationCodes((current) => {
        const next = { ...current }
        if (match) next[field] = match.code
        else delete next[field]
        return next
      })
    } catch {
      setAttention('暂时无法校验该字段。')
    }
  }

  const validateAll = async (): Promise<boolean> => {
    try {
      const fieldErrors = await dependencies.validateAgent(submissionForm(form.value))
      setErrors(mapErrors(fieldErrors))
      setValidationCodes(mapCodes(fieldErrors))
      if (fieldErrors.length > 0) {
        setAttention('配置校验未通过，请检查标记字段。')
        return false
      }
      setAttention('')
      return true
    } catch {
      setAttention('无法完成配置校验。')
      return false
    }
  }

  const save = async (restart: boolean): Promise<void> => {
    if (pending || !(await validateAll())) return
    if (restart && form.dirty && !(await dependencies.confirmRestart())) return

    setPending(true)
    setStatus('')
    try {
      const result = await (restart
        ? dependencies.saveAndRestartAgent(submissionForm(form.value))
        : dependencies.saveAgent(submissionForm(form.value)))
      if (result.saved) {
        const committed = afterSuccessfulSave(form.value)
        form.commit(committed)
        setErrors({})
        setValidationCodes({})
        setStatus(configApplyStatus(result))
        await nodeState?.refresh()
      }
      if (!result.ok) {
        setAttention(operationFailure(result.errorCode))
        return
      }
      setAttention('')
    } catch {
      setAttention('保存失败，请检查配置后重试。')
    } finally {
      setPending(false)
    }
  }

  const runLifecycle = async (action: () => Promise<OperationResult>, success: string): Promise<void> => {
    if (pending) return
    setPending(true)
    setAttention('')
    setStatus('')
    try {
      const result = await action()
      if (result.ok) setStatus(success)
      else if (result.errorCode === 'already_running') setStatus('Agent 已在运行。')
      else {
        if (result.errorCode === 'operation_conflict') void nodeState?.refresh()
        setAttention(operationFailure(result.errorCode))
      }
    } catch {
      setAttention('Agent 操作失败，请稍后重试。')
    } finally {
      setPending(false)
    }
  }

  const copyDiagnostic = async (): Promise<void> => {
    const lines = [
      'Agent 表单诊断',
      `loaded=${loaded}`,
      `dirty=${form.dirty}`,
      `passwordStored=${form.value.database.passwordStored}`,
      `replacePassword=${form.value.database.replacePassword}`,
      `dataDirConfigured=${Boolean(form.value.dataDir)}`,
      `workerExeConfigured=${Boolean(form.value.worker.exePath)}`,
      `thumbCacheConfigured=${Boolean(form.value.thumb.cacheDir)}`,
      `validation=${Object.entries(validationCodes).map(([field, code]) => `${field}:${code}`).join('|') || 'none'}`,
    ]
    try {
      await dependencies.copyText(lines.join('\n'))
      setStatus('配置诊断已复制。')
    } catch {
      setAttention('无法复制配置诊断。')
    }
  }

  if (!loaded) {
    return <section aria-label="Agent 配置"><h2>Agent 配置</h2>{attention ? <p role="alert">{attention}</p> : <p role="status">正在读取配置…</p>}</section>
  }

  const textField = (path: string, label: string, value: string, help?: ReactNode): ReactNode => (
    <FormField id={fieldID(path)} label={label} help={help} error={errors[path]}>
      <input name={path} type="text" value={value} disabled={pending} onChange={(event) => updateField(path, event.currentTarget.value)} onBlur={() => void validateField(path)} />
    </FormField>
  )
  const numberField = (path: string, label: string, value: number, min: number, max: number, step: number, unit: string): ReactNode => (
    <FormField id={fieldID(path)} label={label} help={`${min}–${max} ${unit}`} error={errors[path]}>
      <input name={path} type="number" value={value} min={min} max={max} step={step} disabled={pending} onChange={(event) => updateField(path, Number(event.currentTarget.value))} onBlur={() => void validateField(path)} />
    </FormField>
  )

  return (
    <section aria-label="Agent 配置">
      <h2>Agent 配置</h2>
      <ActionBar ariaLabel="Agent 生命周期操作">
        <button type="button" className="button-primary" disabled={pending || !availableActions.start} onClick={() => void runLifecycle(dependencies.startAgent, 'Agent 启动请求已提交。')}>启动 Agent</button>
        <button type="button" className="button-secondary" disabled={pending || !availableActions.stop} onClick={() => void runLifecycle(dependencies.stopAgent, 'Agent 停止请求已提交。')}>{availableActions.cancelStart ? '取消启动' : '停止 Agent'}</button>
        <button type="button" className="button-secondary" disabled={pending || !availableActions.restart} onClick={() => void runLifecycle(dependencies.restartAgent, 'Agent 重启请求已提交。')}>重启 Agent</button>
      </ActionBar>
      {attention ? <p role="alert">{attention}</p> : null}
      {status ? <p role="status">{status}</p> : null}
      <p>{form.dirty ? '存在未保存更改' : '配置未修改'}</p>

      <form onSubmit={(event) => event.preventDefault()}>
        <fieldset disabled={pending}>
          <legend>常用设置</legend>
          {textField('listenHost', '监听地址', form.value.listenHost)}
          {numberField('listenPort', '监听端口', form.value.listenPort, 1, 65535, 1, '端口')}
          <PathPicker id="data-dir" name="dataDir" label="数据目录" value={form.value.dataDir} error={errors.dataDir} disabled={pending} choosePath={dependencies.choosePath} onChange={(value) => updateField('dataDir', value)} onBlur={() => void validateField('dataDir')} />
          <DatabaseFields value={form.value.database} errors={errors} disabled={pending} onChange={(database) => form.update((current) => new config.AgentForm({ ...current, database }))} onBlurField={(field) => void validateField(field)} />
          {numberField('worker.count', 'Worker 数量', form.value.worker.count, 1, 256, 1, '个')}
        </fieldset>

        <details>
          <summary>高级设置</summary>
          <fieldset disabled={pending}>
            <legend>扫描</legend>
            <FormField id="use-everything" label="扫描所有受支持文件" error={errors.useEverything}>
              <input name="useEverything" type="checkbox" checked={form.value.useEverything} onChange={(event) => updateField('useEverything', event.currentTarget.checked)} onBlur={() => void validateField('useEverything')} />
            </FormField>
            {numberField('scan.hddReadBlockMb', 'HDD 读取块', form.value.scan.hddReadBlockMb, 1, 1024, 1, 'MiB')}
            {numberField('scan.hddStreamsPerDisk', '每磁盘 HDD 流', form.value.scan.hddStreamsPerDisk, 1, 64, 1, '路')}
            {numberField('scan.ssdStreamsPerDisk', '每磁盘 SSD 流', form.value.scan.ssdStreamsPerDisk, 1, 128, 1, '路')}
            {numberField('scan.imageMemResidentMb', '图片常驻内存', form.value.scan.imageMemResidentMb, 0, 1048576, 1, 'MiB')}
            {numberField('scan.imageTimeoutS', '扫描图片超时', form.value.scan.imageTimeoutS, 1, 86400, 1, '秒')}
            {numberField('scan.videoTimeoutS', '扫描视频超时', form.value.scan.videoTimeoutS, 1, 86400, 1, '秒')}
            <TagListField id="scan-image-exts" name="scan.imageExts" label="图片扩展名" values={form.value.scan.imageExts} error={errors['scan.imageExts']} disabled={pending} onChange={(values) => updateField('scan.imageExts', values)} onBlur={() => void validateField('scan.imageExts')} />
            <TagListField id="scan-video-exts" name="scan.videoExts" label="视频扩展名" values={form.value.scan.videoExts} error={errors['scan.videoExts']} disabled={pending} onChange={(values) => updateField('scan.videoExts', values)} onBlur={() => void validateField('scan.videoExts')} />
          </fieldset>

          <fieldset disabled={pending}><legend>同步与协议</legend>
            {numberField('sync.intervalS', '同步间隔', form.value.sync.intervalS, 1, 86400, 1, '秒')}
            {numberField('sync.triggerRows', '同步触发行数', form.value.sync.triggerRows, 1, 10000000, 1, '行')}
            {numberField('sync.upsertBatch', '写入批次', form.value.sync.upsertBatch, 1, 100000, 1, '行')}
            {numberField('proto.heartbeatS', '心跳间隔', form.value.proto.heartbeatS, 1, 3600, 1, '秒')}
          </fieldset>

          <fieldset disabled={pending}><legend>Worker</legend>
            <PathPicker id="worker-exe-path" name="worker.exePath" label="Worker 程序路径" value={form.value.worker.exePath} error={errors['worker.exePath']} disabled={pending} choosePath={dependencies.choosePath} onChange={(value) => updateField('worker.exePath', value)} onBlur={() => void validateField('worker.exePath')} />
            {numberField('worker.imageTimeoutS', 'Worker 图片超时', form.value.worker.imageTimeoutS, 1, 86400, 1, '秒')}
            {numberField('worker.videoTimeoutS', 'Worker 视频超时', form.value.worker.videoTimeoutS, 1, 86400, 1, '秒')}
            {numberField('worker.imageMemoryMb', 'Worker 图片内存', form.value.worker.imageMemoryMb, 0, 1048576, 1, 'MiB')}
            {numberField('worker.respawnDelayMs', 'Worker 重启延迟', form.value.worker.respawnDelayMs, 0, 3600000, 100, '毫秒')}
          </fieldset>

          <fieldset disabled={pending}><legend>管线与缩略图</legend>
            {numberField('pipeline.readChunkKb', '读取块大小', form.value.pipeline.readChunkKb, 1, 1048576, 1, 'KiB')}
            <PathPicker id="thumb-cache-dir" name="thumb.cacheDir" label="缩略图缓存目录" value={form.value.thumb.cacheDir} error={errors['thumb.cacheDir']} disabled={pending} choosePath={dependencies.choosePath} onChange={(value) => updateField('thumb.cacheDir', value)} onBlur={() => void validateField('thumb.cacheDir')} />
            {numberField('thumb.tileMaxSide', '缩略图最大边长', form.value.thumb.tileMaxSide, 1, 16384, 1, '像素')}
            {numberField('thumb.probeTimeoutS', '探测超时', form.value.thumb.probeTimeoutS, 1, 3600, 1, '秒')}
            {numberField('thumb.nativeTimeoutS', '原生计算超时', form.value.thumb.nativeTimeoutS, 1, 86400, 1, '秒')}
            {numberField('thumb.frameTimeoutS', '帧提取超时', form.value.thumb.frameTimeoutS, 1, 86400, 1, '秒')}
          </fieldset>

          <fieldset disabled={pending}><legend>IPC 与删除转发</legend>
            {numberField('ipc.maxFrameMb', 'IPC 最大帧', form.value.ipc.maxFrameMb, 1, 1024, 1, 'MiB')}
            {textField('delete.pipeName', '删除管道名称', form.value.delete.pipeName)}
            {numberField('delete.maxEntriesPerFrame', '每帧最大条目', form.value.delete.maxEntriesPerFrame, 1, 1000000, 1, '条')}
            {numberField('delete.dialTimeoutMs', '删除连接超时', form.value.delete.dialTimeoutMs, 1, 3600000, 1, '毫秒')}
            {numberField('delete.helloTimeoutS', '删除握手超时', form.value.delete.helloTimeoutS, 1, 3600, 1, '秒')}
            {numberField('delete.reportTimeoutS', '删除报告超时', form.value.delete.reportTimeoutS, 1, 86400, 1, '秒')}
          </fieldset>

          <fieldset disabled={pending}><legend>调优</legend>
            <FormField id="tuning-stats-enabled" label="启用统计" error={errors['tuning.statsEnabled']}>
              <input name="tuning.statsEnabled" type="checkbox" checked={form.value.tuning.statsEnabled} onChange={(event) => updateField('tuning.statsEnabled', event.currentTarget.checked)} onBlur={() => void validateField('tuning.statsEnabled')} />
            </FormField>
            {numberField('tuning.statsIntervalS', '统计间隔', form.value.tuning.statsIntervalS, 1, 3600, 1, '秒')}
            {numberField('tuning.statsHistoryS', '统计历史', form.value.tuning.statsHistoryS, 1, 604800, 1, '秒')}
            {numberField('tuning.pendingBytesMb', '待处理字节上限', form.value.tuning.pendingBytesMb, 0, 1048576, 1, 'MiB')}
            {numberField('tuning.statsLogMb', '统计日志上限', form.value.tuning.statsLogMb, 0, 1048576, 1, 'MiB')}
            {textField('tuning.pprofAddr', 'pprof 地址', form.value.tuning.pprofAddr)}
          </fieldset>
        </details>

        <ActionBar ariaLabel="Agent 配置操作">
          <button type="button" className="button-primary" disabled={pending} onClick={() => void save(false)}>保存配置</button>
          <button type="button" className="button-secondary" disabled={pending || !availableActions.restart} onClick={() => void save(true)}>保存并重启 Agent</button>
          <button type="button" className="button-secondary" disabled={pending} onClick={() => form.reset()}>撤销未保存更改</button>
          <button type="button" className="button-secondary" disabled={pending} onClick={() => void copyDiagnostic()}>复制配置诊断</button>
        </ActionBar>
      </form>

      <WorkerSummary ready={workerReady} expected={workerExpected} workers={workers} />
    </section>
  )
}

function withPasswordHidden(source: config.AgentForm): config.AgentForm {
  return new config.AgentForm({ ...source, database: { ...source.database, password: '', replacePassword: false } })
}

function submissionForm(source: config.AgentForm): config.AgentForm {
  return source.database.replacePassword
    ? source
    : new config.AgentForm({ ...source, database: { ...source.database, password: '' } })
}

function afterSuccessfulSave(source: config.AgentForm): config.AgentForm {
  return new config.AgentForm({
    ...source,
    database: {
      ...source.database,
      password: '',
      passwordStored: source.database.passwordStored || (source.database.replacePassword && Boolean(source.database.password)),
      replacePassword: false,
    },
  })
}

function setAtPath<T>(source: T, path: string, value: unknown): T {
  const keys = path.split('.')
  const root = { ...(source as Record<string, unknown>) }
  let cursor = root
  for (let index = 0; index < keys.length - 1; index += 1) {
    const key = keys[index]
    const next = { ...(cursor[key] as Record<string, unknown>) }
    cursor[key] = next
    cursor = next
  }
  cursor[keys[keys.length - 1]] = value
  return root as T
}

function fieldID(path: string): string {
  return path.replaceAll('.', '-')
}

function safeMessage(message: string, form: config.AgentForm): string {
  let safe = String(message || '')
    .replace(/(?:postgres(?:ql)?|mysql):\/\/\S+/gi, '[连接信息已隐藏]')
    .replace(/[A-Za-z]:[\\/][^\s,;]*/g, '[路径已隐藏]')
  const sensitive = [form.database.password, form.database.user, form.database.host, form.database.database, form.dataDir, form.worker.exePath, form.thumb.cacheDir]
    .filter((value) => value.length >= 3)
  for (const value of sensitive) safe = safe.replaceAll(value, '[敏感内容已隐藏]')
  safe = safe.trim().slice(0, 160)
  return safe || '字段值无效。'
}

function operationFailure(code: string): string {
  return code ? `操作失败（${code.replace(/[^a-z0-9_-]/gi, '').slice(0, 48) || 'unknown'}）。` : '操作失败，请检查配置后重试。'
}

function configApplyStatus(result: ConfigApplyResult): string {
  const digest = result.sha256 ? `（${result.sha256.slice(0, 12)}）` : ''
  if (result.needsRestart && !result.restarted) return `配置已保存，需要重启后生效${digest}。`
  if (result.restarted) return `配置已保存并重启${digest}。`
  return `配置已保存${digest}。`
}
