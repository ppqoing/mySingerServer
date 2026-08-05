import { useEffect, useState, type ReactNode } from 'react'
import { GetTraySettings, OpenLocation, SaveTraySettings } from '../../wailsjs/go/main/Backend'
import { traymodel } from '../../wailsjs/go/models'
import { ActionBar } from '../components/ActionBar'
import { FormField } from '../components/FormField'
import { useOptionalNodeState } from '../state/NodeStateContext'
import { useDirtyForm } from '../state/useDirtyForm'

type OperationResult = { ok: boolean; errorCode: string; errorSummary: string; uacCancelled: boolean }
type LocationKind = 'agent-logs' | 'helper-logs' | 'agent-backup' | 'helper-backup'

export type SettingsPageDependencies = {
  getTraySettings: () => Promise<traymodel.TraySettings>
  saveTraySettings: (value: traymodel.TraySettings) => Promise<OperationResult>
  openLocation: (kind: LocationKind) => Promise<OperationResult>
}

type SettingsPageProps = {
  dependencies?: SettingsPageDependencies
  onDirtyChange: (dirty: boolean) => void
  onRequestExit: () => void
}

const productionDependencies: SettingsPageDependencies = {
  getTraySettings: GetTraySettings,
  saveTraySettings: SaveTraySettings,
  openLocation: OpenLocation,
}

const emptySettings = new traymodel.TraySettings({
  loginStartTray: false,
  agentStartMode: 'manual',
  helperEnabled: false,
  helperStartMode: 'manual',
  closeToTray: true,
  refreshIntervalSeconds: 2,
  notificationLevel: 'important',
})

export function SettingsPage({ dependencies = productionDependencies, onDirtyChange, onRequestExit }: SettingsPageProps): ReactNode {
  const nodeState = useOptionalNodeState()
  const form = useDirtyForm(emptySettings)
  const [loaded, setLoaded] = useState(false)
  const [pending, setPending] = useState(false)
  const [attention, setAttention] = useState('')
  const [status, setStatus] = useState('')
  const commit = form.commit

  useEffect(() => {
    let active = true
    dependencies.getTraySettings().then((value) => {
      if (!active) return
      commit(new traymodel.TraySettings(value))
      setLoaded(true)
    }).catch(() => {
      if (active) setAttention('无法读取程序设置。')
    })
    return () => { active = false }
  }, [commit, dependencies])

  useEffect(() => {
    onDirtyChange(form.dirty)
  }, [form.dirty, onDirtyChange])

  const update = (change: Partial<traymodel.TraySettings>): void => {
    form.update((current) => new traymodel.TraySettings({ ...current, ...change }))
    setStatus('')
  }

  const save = async (): Promise<void> => {
    if (pending) return
    const normalized = new traymodel.TraySettings({
      ...form.value,
      helperStartMode: form.value.helperEnabled ? form.value.helperStartMode : 'manual',
    })
    setPending(true)
    setAttention('')
    try {
      const result = await dependencies.saveTraySettings(normalized)
      if (!result.ok) {
        setAttention('保存失败，已重新读取实际设置。')
        await reloadActualSettings()
        return
      }
      form.commit(normalized)
      await nodeState?.refresh()
      setStatus('程序设置已保存。')
    } catch {
      setAttention('保存失败，请检查设置后重试。')
      await reloadActualSettings()
    } finally {
      setPending(false)
    }
  }

  const reloadActualSettings = async (): Promise<void> => {
    try {
      const actual = await dependencies.getTraySettings()
      form.commit(new traymodel.TraySettings(actual))
    } catch {
      setAttention('保存失败，且无法重新读取实际设置。')
    }
  }

  const open = async (kind: LocationKind): Promise<void> => {
    if (pending) return
    setAttention('')
    try {
      const result = await dependencies.openLocation(kind)
      if (!result.ok) setAttention('无法打开固定位置。')
    } catch {
      setAttention('无法打开固定位置。')
    }
  }

  if (!loaded) {
    return <section aria-label="程序设置"><h2>程序设置</h2>{attention ? <p role="alert">{attention}</p> : <p role="status">正在读取程序设置…</p>}</section>
  }

  return (
    <section aria-label="程序设置">
      <h2>程序设置</h2>
      {attention ? <p role="alert">{attention}</p> : null}
      {status ? <p role="status">{status}</p> : null}
      <form onSubmit={(event) => event.preventDefault()}>
        <fieldset disabled={pending}>
          <legend>启动</legend>
          <FormField id="settings-login-start" label="登录后启动托盘程序">
            <input id="settings-login-start" type="checkbox" checked={form.value.loginStartTray} onChange={(event) => update({ loginStartTray: event.currentTarget.checked })} />
          </FormField>
          <FormField id="settings-agent-mode" label="Agent 启动方式">
            <select id="settings-agent-mode" value={form.value.agentStartMode} onChange={(event) => update({ agentStartMode: event.currentTarget.value })}>
              <option value="manual">手动</option><option value="automatic">自动</option>
            </select>
          </FormField>
          <FormField id="settings-helper-enabled" label="启用 Helper">
            <input id="settings-helper-enabled" type="checkbox" checked={form.value.helperEnabled} onChange={(event) => update({ helperEnabled: event.currentTarget.checked, ...(!event.currentTarget.checked ? { helperStartMode: 'manual' } : {}) })} />
          </FormField>
          <FormField id="settings-helper-mode" label="Helper 启动方式">
            <select id="settings-helper-mode" value={form.value.helperStartMode} disabled={!form.value.helperEnabled} onChange={(event) => update({ helperStartMode: event.currentTarget.value })}>
              <option value="manual">手动</option><option value="automatic">自动</option>
            </select>
          </FormField>
        </fieldset>
        <fieldset disabled={pending}>
          <legend>窗口与状态</legend>
          <FormField id="settings-close-to-tray" label="关闭窗口时隐藏到托盘">
            <input id="settings-close-to-tray" type="checkbox" checked={form.value.closeToTray} onChange={(event) => update({ closeToTray: event.currentTarget.checked })} />
          </FormField>
          <FormField id="settings-refresh" label="状态刷新间隔">
            <select id="settings-refresh" value={form.value.refreshIntervalSeconds} onChange={(event) => update({ refreshIntervalSeconds: Number(event.currentTarget.value) })}>
              <option value="1">1 秒</option><option value="2">2 秒</option><option value="3">3 秒</option>
            </select>
          </FormField>
          <FormField id="settings-notification" label="通知级别">
            <select id="settings-notification" value={form.value.notificationLevel} onChange={(event) => update({ notificationLevel: event.currentTarget.value })}>
              <option value="important">重要通知</option><option value="all">全部通知</option>
            </select>
          </FormField>
        </fieldset>
        <ActionBar ariaLabel="程序设置操作">
          <button type="button" className="button-primary" disabled={pending} onClick={() => void save()}>保存程序设置</button>
          <button type="button" className="button-secondary" disabled={pending || !form.dirty} onClick={() => form.reset()}>撤销未保存更改</button>
          <button type="button" className="button-secondary" onClick={onRequestExit}>退出托盘程序</button>
        </ActionBar>
      </form>
      <ActionBar ariaLabel="固定位置">
        <button type="button" className="button-secondary" onClick={() => void open('agent-logs')}>打开 Agent 日志</button>
        <button type="button" className="button-secondary" onClick={() => void open('helper-logs')}>打开 Helper 日志</button>
        <button type="button" className="button-secondary" onClick={() => void open('agent-backup')}>打开 Agent 配置备份</button>
        <button type="button" className="button-secondary" onClick={() => void open('helper-backup')}>打开 Helper 配置备份</button>
      </ActionBar>
    </section>
  )
}
