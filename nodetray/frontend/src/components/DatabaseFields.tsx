import type { ReactNode } from 'react'
import type { config } from '../../wailsjs/go/models'
import { ActionBar } from './ActionBar'
import { FormField } from './FormField'

type DatabaseFieldsProps = {
  value: config.DatabaseForm
  errors: Readonly<Record<string, string>>
  disabled?: boolean
  onChange: (value: config.DatabaseForm) => void
  onBlurField: (field: string) => void
}

export function DatabaseFields({ value, errors, disabled = false, onChange, onBlurField }: DatabaseFieldsProps): ReactNode {
  const set = <K extends keyof config.DatabaseForm>(key: K, next: config.DatabaseForm[K]): void => {
    onChange({ ...value, [key]: next })
  }
  const replacePassword = (): void => set('replacePassword', true)
  const cancelPassword = (): void => onChange({ ...value, password: '', replacePassword: false })

  return (
    <fieldset>
      <legend>数据库</legend>
      <FormField id="database-host" label="数据库主机" error={errors['database.host']}>
        <input name="database.host" type="text" value={value.host} disabled={disabled} onChange={(event) => set('host', event.currentTarget.value)} onBlur={() => onBlurField('database.host')} />
      </FormField>
      <FormField id="database-port" label="数据库端口" help="1–65535" error={errors['database.port']}>
        <input name="database.port" type="number" min={1} max={65535} step={1} value={value.port} disabled={disabled} onChange={(event) => set('port', Number(event.currentTarget.value))} onBlur={() => onBlurField('database.port')} />
      </FormField>
      <FormField id="database-name" label="数据库名称" error={errors['database.database']}>
        <input name="database.database" type="text" value={value.database} disabled={disabled} onChange={(event) => set('database', event.currentTarget.value)} onBlur={() => onBlurField('database.database')} />
      </FormField>
      <FormField id="database-user" label="数据库用户" error={errors['database.user']}>
        <input name="database.user" type="text" value={value.user} disabled={disabled} autoComplete="off" onChange={(event) => set('user', event.currentTarget.value)} onBlur={() => onBlurField('database.user')} />
      </FormField>
      <FormField
        id="database-password"
        label="数据库密码"
        help={value.passwordStored && !value.replacePassword ? '已保存，留空保留' : '密码不会显示在诊断或摘要中'}
        error={errors['database.password']}
      >
        <input
          name="database.password"
          type="password"
          value={value.password}
          disabled={disabled || !value.replacePassword}
          autoComplete="new-password"
          onChange={(event) => set('password', event.currentTarget.value)}
          onBlur={() => onBlurField('database.password')}
        />
      </FormField>
      <ActionBar ariaLabel="数据库密码操作">
        {value.replacePassword ? (
          <button type="button" className="button-secondary" disabled={disabled} onClick={cancelPassword}>取消替换密码</button>
        ) : (
          <button type="button" className="button-secondary" disabled={disabled} onClick={replacePassword}>替换密码</button>
        )}
      </ActionBar>
      <FormField id="database-ssl-mode" label="数据库 SSL 模式" error={errors['database.sslMode']}>
        <select name="database.sslMode" value={value.sslMode} disabled={disabled} onChange={(event) => set('sslMode', event.currentTarget.value)} onBlur={() => onBlurField('database.sslMode')}>
          <option value="disable">disable</option>
          <option value="prefer">prefer</option>
          <option value="require">require</option>
          <option value="verify-ca">verify-ca</option>
          <option value="verify-full">verify-full</option>
        </select>
      </FormField>
    </fieldset>
  )
}
