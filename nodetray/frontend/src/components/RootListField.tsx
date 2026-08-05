import { useState, type ReactNode } from 'react'

type RootListFieldProps = {
  id: string
  name: string
  label: string
  values: string[]
  errors?: Record<string, string>
  disabled?: boolean
  chooseRoot?: () => Promise<string>
  onChange: (values: string[]) => void
}

export function RootListField({
  id,
  name,
  label,
  values,
  errors = {},
  disabled = false,
  chooseRoot,
  onChange,
}: RootListFieldProps): ReactNode {
  const [manualRoot, setManualRoot] = useState('')

  const appendRoot = (candidate: string): boolean => {
    const selected = candidate.trim()
    if (!selected || !isLocalAbsolutePath(selected) || values.includes(selected)) return false
    onChange([...values, selected])
    return true
  }

  const addManualRoot = (): void => {
    if (appendRoot(manualRoot)) setManualRoot('')
  }

  const addRoot = async (): Promise<void> => {
    if (!chooseRoot) return
    appendRoot(await chooseRoot())
  }

  return (
    <div id={id} className="form-field" role="group" aria-label={label}>
      <span className="form-field__label">{label}</span>
      {errors[name] ? <p className="form-field__error" role="alert">{errors[name]}</p> : null}
      <ul aria-label={`${label}列表`}>
        {values.map((value, index) => {
          const itemError = errors[`${name}[${index}]`] ?? errors[`${name}.${index}`]
          return (
            <li key={`${value}-${index}`} aria-label={value}>
              <code>{value}</code>
              {itemError ? <p className="form-field__error" role="alert">{itemError}</p> : null}
              <button
                className="button-secondary"
                type="button"
                disabled={disabled}
                aria-label={`移除 ${value}`}
                onClick={() => onChange(values.filter((_, itemIndex) => itemIndex !== index))}
              >
                移除
              </button>
            </li>
          )
        })}
      </ul>
      <label htmlFor={`${id}-manual`}>手动输入{label}</label>
      <input
        id={`${id}-manual`}
        name={`${name}Manual`}
        type="text"
        value={manualRoot}
        disabled={disabled}
        onChange={(event) => setManualRoot(event.currentTarget.value)}
      />
      <button className="button-secondary" type="button" disabled={disabled} aria-label={`添加${label}`} onClick={addManualRoot}>
        添加
      </button>
      {chooseRoot ? (
        <button className="button-secondary" type="button" disabled={disabled} aria-label={`选择${label}`} onClick={() => void addRoot()}>
          选择目录…
        </button>
      ) : null}
    </div>
  )
}

function isLocalAbsolutePath(value: string): boolean {
  return /^[A-Za-z]:[\\/](?:[^\\/]+(?:[\\/]|$))*$/.test(value)
    || /^\\\\[^\\/]+\\[^\\/]+(?:\\[^\\/]+)*\\?$/.test(value)
}
