import { useState, type KeyboardEvent, type ReactNode } from 'react'
import { FormField } from './FormField'

type TagListFieldProps = {
  id: string
  name: string
  label: string
  values: string[]
  error?: string
  disabled?: boolean
  onChange: (values: string[]) => void
  onBlur?: () => void
}

export function TagListField({
  id,
  name,
  label,
  values,
  error,
  disabled = false,
  onChange,
  onBlur,
}: TagListFieldProps): ReactNode {
  const [entry, setEntry] = useState('')

  const add = (): void => {
    const normalized = normalizeExtension(entry)
    setEntry('')
    if (normalized && !values.includes(normalized)) {
      onChange([...values, normalized])
    }
  }

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'Enter' || event.key === ',') {
      event.preventDefault()
      add()
    } else if (event.key === 'Backspace' && !entry && values.length > 0) {
      onChange(values.slice(0, -1))
    }
  }

  return (
    <div className="tag-list-field">
      <FormField id={id} label={label} help="输入扩展名后按 Enter 添加" error={error}>
        <input
          name={name}
          type="text"
          value={entry}
          disabled={disabled}
          onChange={(event) => setEntry(event.currentTarget.value)}
          onKeyDown={onKeyDown}
          onBlur={() => {
            add()
            onBlur?.()
          }}
        />
      </FormField>
      <div className="tag-list" aria-label={`${label}列表`}>
        {values.map((value) => (
          <span className="tag" key={value}>
            {value}
            <button
              type="button"
              disabled={disabled}
              aria-label={`删除 ${value}`}
              onClick={() => onChange(values.filter((item) => item !== value))}
            >
              ×
            </button>
          </span>
        ))}
      </div>
    </div>
  )
}

function normalizeExtension(value: string): string {
  const trimmed = value.trim().toLowerCase().replace(/^\.+/, '')
  if (!/^[a-z0-9][a-z0-9+_-]*$/.test(trimmed)) {
    return ''
  }
  return `.${trimmed}`
}
